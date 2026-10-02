// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// Usage 返回卷级已用字节（写入累计 - 删除释放；loadIndex 按 meta size 累计，重启正确）。
func (s *SecretdataFS) Usage() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.usage
}

// CurrentVersion 返回卷当前乐观锁版本（多进程 CAS 基版本）。
func (s *SecretdataFS) CurrentVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.volVersion
}

// writeFileDedup 去重写路径：整文件内容哈希查询卷级池 → 命中引用（不重复加密/上传），
// 未命中首次加密并登记池。数据分块恒在卷级 dedupDir，文件 meta blob 在自身 container。
// 失败统一回收本次新建容器（created）。池命中判定与引用预留同持锁（I-3）。
func (s *SecretdataFS) writeFileDedup(ctx context.Context, rel string, data []byte, container string, created []dirCreation, mtime, sv, expected int64) (err error) {
	defer func() {
		if err != nil {
			s.pruneCreatedDirs(ctx, created)
		}
	}()
	key, _ := shardseal.Hash16(data) // 整文件内容 SHA-16hex（== 单分块 OrigSHA256，重启后可重建）
	dir, err := s.ensureDedupDir()
	if err != nil {
		return err
	}
	mt := s.blobMTime(mtime)
	s.mu.Lock()
	b, ok := s.dedupPool[key]
	if !ok || b.meta == nil {
		s.mu.Unlock()
		return s.writeFileDedupMiss(ctx, rel, data, key, dir, container, mtime, mt, sv, expected)
	}
	b.refs++ // 原子预留引用（锁内）：上传/提交失败再回滚，防并发 last-ref 删后悬空
	tpl := b.meta
	s.mu.Unlock()
	return s.writeFileDedupHit(ctx, rel, data, key, dir, container, mtime, mt, sv, expected, tpl)
}

// ensureDedupDir 确保卷级去重容器存在（懒创建一次），返回其容器名。
func (s *SecretdataFS) ensureDedupDir() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedupDir != "" {
		return s.dedupDir, nil
	}
	c, err := shardseal.RandDirName()
	if err != nil {
		return "", err
	}
	s.dedupDir = c
	return c, nil
}

// writeFileDedupHit 池命中（引用已预留）：克隆模板 meta（Salt/Chunks/Extra 随模板），
// 改名后加密 meta 上传；失败/冲突回滚预留引用。
func (s *SecretdataFS) writeFileDedupHit(ctx context.Context, rel string, data []byte, key, dir, container string, mtime, mt, sv, expected int64, tpl *shardseal.Meta) error {
	ref := cloneMeta(tpl)
	ref.Original.Name = path.Base(rel)
	ref.Original.MTime = mtimeString(mtime)
	name, blob, err := s.encryptMetaBlob(ref)
	if err != nil {
		s.releasePoolRef(key)
		return err
	}
	metaPath := path.Join(container, name)
	if werr := s.uploadBlob(ctx, metaPath, blob, mt); werr != nil {
		s.releasePoolRef(key)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	if cerr := s.commitDedupEntry(ctx, rel, &metaEntry{
		size: int64(len(data)), mtime: mtime, dirSeg: container, metaName: name, meta: ref, dataDir: dir,
	}, int64(len(data)), sv, expected, []string{metaPath}); cerr != nil {
		s.releasePoolRef(key) // 提交失败/版本冲突：释放预留
		return cerr
	}
	return nil
}

// writeFileDedupMiss 池未命中：单分块加密，分块上传到池容器 + meta 上传到自身容器；
// 锁内登记池条目（refs=1）。并发未命中（同内容双首写）时保留各自私有分块，不覆盖他人。
func (s *SecretdataFS) writeFileDedupMiss(ctx context.Context, rel string, data []byte, key, dir, container string, mtime, mt, sv, expected int64) error {
	o, chunkBytes, err := s.encryptContentSingle(data, rel)
	if err != nil {
		return err
	}
	chunkName := o.ChunkNames[0]
	chunkPath := path.Join(dir, chunkName)
	if werr := s.uploadBlob(ctx, chunkPath, chunkBytes, mt); werr != nil {
		return fmt.Errorf("secretdata: 上传去重分块失败: %w", werr)
	}
	meta := o.Meta
	meta.Original.Name = path.Base(rel)
	meta.Original.MTime = mtimeString(mtime)
	if meta.Extra == nil {
		meta.Extra = map[string][]byte{}
	}
	meta.Extra["dedup"] = []byte(dir)
	name, metaBlob, err := s.encryptMetaBlob(meta)
	if err != nil {
		_ = s.inner.Delete(ctx, chunkPath)
		return err
	}
	metaPath := path.Join(container, name)
	if werr := s.uploadBlob(ctx, metaPath, metaBlob, mt); werr != nil {
		_ = s.inner.Delete(ctx, chunkPath)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	s.mu.Lock()
	if expected >= 0 && s.volVersion != sv {
		s.mu.Unlock()
		_ = s.inner.Delete(ctx, chunkPath)
		_ = s.inner.Delete(ctx, metaPath)
		return versionConflictErr(sv, expected)
	}
	// 并发未命中（同 ID 双首写）：已有池条目则保留其 blob 作他人共享，本文件持自有
	// 分块（不覆盖 —— 覆盖会让他人 blob 丢引用）。entry 引用分块仍在本容器 dataDir。
	if _, exists := s.dedupPool[key]; !exists {
		s.dedupPool[key] = &dedupBlob{name: chunkName, refs: 1, meta: meta}
	}
	newVer := s.volVersion + 1
	prev := s.index[rel]
	s.index[rel] = &metaEntry{size: int64(len(data)), mtime: mtime, dirSeg: container, metaName: name, meta: meta, dataDir: dir}
	addDirKeysLocked(s.dirs, rel)
	s.volVersion = newVer
	s.usage += int64(len(data))
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	s.mu.Unlock()
	if prev != nil {
		s.removeVersionMeta(prev)
	}
	return nil
}

// commitDedupEntry 提交去重条目：持锁 CAS（expected ≥0 须 volVersion==sv），推进版本/
// usage。失败清理本次上传路径并返回版本冲突错误（调用方负责回滚池预留引用）。
func (s *SecretdataFS) commitDedupEntry(ctx context.Context, rel string, e *metaEntry, newSize, sv, expected int64, cleanPaths []string) error {
	s.mu.Lock()
	if expected >= 0 && s.volVersion != sv {
		s.mu.Unlock()
		for _, p := range cleanPaths {
			_ = s.inner.Delete(ctx, p)
		}
		return versionConflictErr(sv, expected)
	}
	newVer := s.volVersion + 1
	prev := s.index[rel]
	s.index[rel] = e
	e.baseVersion = newVer
	addDirKeysLocked(s.dirs, rel)
	s.volVersion = newVer
	s.usage += newSize
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	s.mu.Unlock()
	if prev != nil {
		s.removeVersionMeta(prev)
	}
	return nil
}

// releasePoolRef 释放去重池预留引用：归零时物理删 blob 并删除池条目。
func (s *SecretdataFS) releasePoolRef(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.dedupPool[key]
	if !ok {
		return
	}
	b.refs--
	if b.refs <= 0 {
		if s.dedupDir != "" {
			_ = s.inner.Delete(context.Background(), path.Join(s.dedupDir, b.name))
		}
		delete(s.dedupPool, key)
	}
}

// versionConflictErr 构造版本冲突错误（哨兵包装）。
func versionConflictErr(sv, expected int64) error {
	return fmt.Errorf("secretdata: 版本冲突：卷当前 %d，调用方期望 %d: %w", sv, expected, ErrVersionConflict)
}

// uploadBlob 上传数据/元数据字节到底层（mtime 打散由调用方给定 mt）。
func (s *SecretdataFS) uploadBlob(ctx context.Context, p string, data []byte, mt int64) error {
	return s.inner.WriteFile(ctx, p, bytes.NewReader(data), int64(len(data)), mt)
}

// encryptContentSingle 把整文件内容按单分块策略加密，返回 EncryptionResult 与该分块
// 字节（去重池 blob 载体；单分块 OrigSHA256 == 整文件内容哈希）。走 EncryptShardsBytes
// 内存变体（Imp-2 部分修复：不再写临时源 + 二次整读，峰值 1× 文件）。
func (s *SecretdataFS) encryptContentSingle(data []byte, rel string) (*shardseal.EncryptionResult, []byte, error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("secretdata: 空内容不可去重")
	}
	tmp, err := os.MkdirTemp(s.temp, "chunks-dedup-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	bmin := int64(16)
	if int64(len(data)) < bmin {
		bmin = int64(len(data))
	}
	policy := shardseal.BlockPolicy{Mode: "random", Min: int64(len(data)), Max: int64(len(data)), BlockletMin: bmin, BlockletMax: int64(len(data))}
	out, err := shardseal.EncryptShardsBytes(data, sanitizeName(rel), tmp, s.secret, policy, s.metaPadTarget(), s.algoVer)
	if err != nil {
		return nil, nil, fmt.Errorf("secretdata: 去重单块分块加密失败: %w", err)
	}
	blob, rerr := os.ReadFile(filepath.Join(tmp, out.ChunkNames[0]))
	if rerr != nil {
		return nil, nil, rerr
	}
	return out, blob, nil
}

// encryptMetaBlob 把 meta 明文加密为统一格式 blob 并锚定其名字（三段哈希真实，含 padding）。
// 用于去重引用/墓碑 meta 落盘（EncryptShards 之外的自构造 meta 也走同格式）。
func (s *SecretdataFS) encryptMetaBlob(m *shardseal.Meta) (string, []byte, error) {
	salt, err := base64.StdEncoding.DecodeString(m.Salt)
	if err != nil {
		return "", nil, fmt.Errorf("secretdata: meta salt 解码失败: %w", err)
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt, s.algoVer)
	if kerr != nil {
		return "", nil, kerr
	}
	metaJSON, jerr := json.Marshal(m)
	if jerr != nil {
		return "", nil, jerr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, metaJSON, s.metaPadTarget())
	if berr != nil {
		return "", nil, berr
	}
	orig, _ := shardseal.Hash16(metaJSON)
	enc, _ := shardseal.Hash16(blob)
	total := ""
	if len(m.Original.SHA256) >= 16 {
		total = m.Original.SHA256[:16]
	}
	return shardseal.MetaName(orig, total, enc), blob, nil
}

// cloneMeta 浅拷贝 meta（Chunks 切片独立；内容只读不共享变更）。
func cloneMeta(m *shardseal.Meta) *shardseal.Meta {
	if m == nil {
		return nil
	}
	cp := *m
	cp.Chunks = append([]shardseal.ChunkInfo(nil), m.Chunks...)
	return &cp
}

// ---- GC ----

// gcKill 是一次物理删除操作（容器 + 文件名）。
type gcKill struct {
	container, name string
}

// GC 清理墓碑与孤立分块，返回删除文件数。两阶段（先标记存活引用、再扫未引用分块）
// 消除「分块在 file meta 之前被遍历到→误删」的排序依赖（C-1）。全程持 s.mu.Lock：
// 若存在在途写则本轮放弃（writesInFlight>0），持锁期间无新写插入 → 不会删除在途未
// 提交分块（I-2）。引用集合按「容器/名」限定，去重池引用另以 basename 兜底保护。
//
// Imp-3：标记阶段不再对底层逐 meta 重读盘 + 重跑 scrypt（原 gcMarkFileMeta 对每 meta
// 重新 readBlob + decryptBlob→DeriveKey，128MB/次，大卷一轮 GC = 长时间全卷阻塞）。
// 挂载态 s.index 是存活 meta 的完整解密镜像（writesInFlight 门禁已排干在途写，挂载态
// 与磁盘一致），故标记直接遍历 s.index 收集存活 meta/分块/parity 引用，零磁盘读 + 零
// scrypt；磁盘扫描只做第二遍孤儿判定。锁持有粒度仍为整卷（writesInFlight 门禁要求），
// 但不再有重派生开销，一轮 GC 对活跃文件量 O(1) 解密、仅按需扫盘。
func (s *SecretdataFS) GC(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writesInFlight > 0 {
		return 0, nil // 在途写存在：放弃本轮，避免删未提交分块
	}
	containers := s.gcContainersLocked()
	poolRefs := s.gcPoolNamesLocked()
	// 存活引用 key: container + "/" + name（meta blob 与分块均以自包含容器定位）。
	referenced := map[string]struct{}{}
	// 存活的文件 meta 名（直接取自 index；自产生不被扫描，仅作内存判定用）。
	s.gcMarkIndex(ctx, referenced)
	kills := []gcKill{}
	for _, c := range containers {
		s.gcSweepContainer(ctx, c, referenced, poolRefs, &kills)
	}
	for _, k := range kills {
		_ = s.inner.Delete(ctx, path.Join(k.container, k.name))
	}
	return len(kills), nil
}

// gcMarkIndex 从内存索引直接标记存活引用（Imp-3 核心）：遍历 s.index 全部活跃条目，
// 把其 meta blob（dirSeg/metaName）、全部分块（dataDir 或 dirSeg）与 parity blob 登记到
// referenced；并为墓碑/旧版本 meta 名登记「需删除」路径（惰性：扫描时按未引用判删）。
// 不在需持锁后…s.index 在持锁下是完整镜像（与磁盘一致）。调用方持 s.mu.Lock。
func (s *SecretdataFS) gcMarkIndex(ctx context.Context, referenced map[string]struct{}) {
	for _, e := range s.index {
		if e == nil || e.meta == nil {
			continue
		}
		// meta blob 自身存活引用（在 dirSeg）；墓碑（Deleted）条目自索引清理时已删，
		// 不在 index —— 旧墓碑由第二遍扫 KindFileMeta 未命中而删。
		isDeleted := e.meta.Deleted
		// 数据分块所在容器：去重引用文件用 Extra["dedup"]（dataDir），否则 dirSeg。
		dir := e.dataDir
		if dir == "" {
			dir = e.dirSeg
		}
		for _, ci := range e.meta.Chunks {
			referenced[dir+"/"+ci.FileName] = struct{}{}
		}
		if p := e.meta.Parity; p != nil {
			referenced[dir+"/"+p.FileName] = struct{}{}
		}
		// 存活文件 meta blob 自身加入引用（防第二遍把活 meta 当孤儿扫掉）。
		if !isDeleted {
			referenced[e.dirSeg+"/"+e.metaName] = struct{}{}
		}
	}
}

// gcSweepContainer 第二遍：删未引用且非池引用的孤立分块 + 未被索引引用的文件 meta
// （墓碑/旧版本残留）。目录 meta（@）恒保留。原 Imp-3 旧的 gcMarkFileMeta 已不再需要
// 对每个 meta 重读盘 + 重 scrypt——存活引用由 gcMarkIndex 从内存镜像提供。
func (s *SecretdataFS) gcSweepContainer(ctx context.Context, c string, referenced, poolRefs map[string]struct{}, kills *[]gcKill) {
	entries, err := s.inner.ListDir(ctx, c)
	if err != nil {
		return
	}
	for _, f := range entries {
		if f.IsDir {
			continue
		}
		switch shardseal.ClassifyName(f.Name) {
		case shardseal.KindChunk:
			if _, ok := referenced[c+"/"+f.Name]; ok {
				continue
			}
			if _, isPool := poolRefs[f.Name]; isPool {
				continue
			}
			*kills = append(*kills, gcKill{c, f.Name})
		case shardseal.KindFileMeta:
			// 文件 meta：索引无该名 → 墓碑或旧版本残留（无任何 live 引用）→ 删除。
			if _, ok := referenced[c+"/"+f.Name]; ok {
				continue
			}
			*kills = append(*kills, gcKill{c, f.Name})
		case shardseal.KindDirMeta:
			// 目录 meta 恒保留（目录结构由 dirMeta 持久化）。
		}
	}
}

// gcContainersLocked 返回全部容器（调用方已持 s.mu）。
func (s *SecretdataFS) gcContainersLocked() []string {
	seen := map[string]struct{}{}
	for _, c := range s.dirSegs {
		seen[c] = struct{}{}
	}
	if s.dedupDir != "" {
		seen[s.dedupDir] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	return out
}

// gcPoolNamesLocked 快照去重池存活 blob 名（引用计数>0，basename 由调用方持锁读）。
func (s *SecretdataFS) gcPoolNamesLocked() map[string]struct{} {
	out := map[string]struct{}{}
	for _, b := range s.dedupPool {
		if b.refs > 0 {
			out[b.name] = struct{}{}
		}
	}
	return out
}

// startGC 后台周期 GC：独立 goroutine + time.Ticker。仅 opts.GCInterval>0 时由 NewFS
// 启动；ctx 取消即退出（backend.Close 取消）。
func (s *SecretdataFS) startGC(ctx context.Context) {
	go func() {
		interval := s.opts.GCInterval
		if interval <= 0 {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.GC(ctx)
			}
		}
	}()
}
