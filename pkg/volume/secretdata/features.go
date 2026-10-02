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
func (s *SecretdataFS) writeFileDedup(ctx context.Context, rel string, data []byte, container string, mtime, next int64) error {
	key, _ := shardseal.Hash16(data) // 整文件内容 SHA-16hex（== 单分块 OrigSHA256，重启后可重建）
	dir, err := s.ensureDedupDir()
	if err != nil {
		return err
	}
	mt := s.blobMTime(mtime)
	if s.hasPool(key) {
		return s.writeFileDedupHit(ctx, rel, data, key, dir, container, mtime, mt, next)
	}
	return s.writeFileDedupMiss(ctx, rel, data, key, dir, container, mtime, mt, next)
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

// hasPool 报告内容哈希是否已登记进去重池（锁内读）。
func (s *SecretdataFS) hasPool(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dedupPool[key] != nil
}

// writeFileDedupHit 池命中：克隆模板 meta（Salt/Chunks/Extra 随模板），改名后加密 meta
// 上传到自身 container；引用计数 +1。
func (s *SecretdataFS) writeFileDedupHit(ctx context.Context, rel string, data []byte, key, dir, container string, mtime, mt, next int64) error {
	tpl, ok := s.poolTemplate(key)
	if !ok || tpl == nil {
		return s.writeFileDedupMiss(ctx, rel, data, key, dir, container, mtime, mt, next)
	}
	ref := cloneMeta(tpl)
	ref.Original.Name = path.Base(rel)
	ref.Original.MTime = mtimeString(mtime)
	name, blob, err := s.encryptMetaBlob(ref)
	if err != nil {
		return err
	}
	if werr := s.uploadBlob(ctx, path.Join(container, name), blob, mt); werr != nil {
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	s.mu.Lock()
	if b, ok := s.dedupPool[key]; ok {
		b.refs++
	}
	prev := s.index[rel]
	s.index[rel] = &metaEntry{size: int64(len(data)), mtime: mtime, dirSeg: container, metaName: name, meta: ref, dataDir: dir, baseVersion: next}
	addDirKeysLocked(s.dirs, rel)
	s.commitVersionLocked(next, int64(len(data)), prev)
	s.mu.Unlock()
	if prev != nil {
		s.removeVersionMeta(prev)
	}
	return nil
}

// writeFileDedupMiss 池未命中：整文件单分块加密，分块上传到池容器 + meta 上传到自身
// container；登记池条目（refs=1）。
func (s *SecretdataFS) writeFileDedupMiss(ctx context.Context, rel string, data []byte, key, dir, container string, mtime, mt, next int64) error {
	o, chunkBytes, err := s.encryptContentSingle(data, rel)
	if err != nil {
		return err
	}
	chunkName := o.ChunkNames[0]
	if werr := s.uploadBlob(ctx, path.Join(dir, chunkName), chunkBytes, mt); werr != nil {
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
		return err
	}
	if werr := s.uploadBlob(ctx, path.Join(container, name), metaBlob, mt); werr != nil {
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	s.mu.Lock()
	s.dedupPool[key] = &dedupBlob{name: chunkName, refs: 1, meta: meta}
	prev := s.index[rel]
	s.index[rel] = &metaEntry{size: int64(len(data)), mtime: mtime, dirSeg: container, metaName: name, meta: meta, dataDir: dir, baseVersion: next}
	addDirKeysLocked(s.dirs, rel)
	s.commitVersionLocked(next, int64(len(data)), prev)
	s.mu.Unlock()
	if prev != nil {
		s.removeVersionMeta(prev)
	}
	return nil
}

// poolTemplate 读取去重条目模板 meta（锁内读）。
func (s *SecretdataFS) poolTemplate(key string) (*shardseal.Meta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.dedupPool[key]
	if !ok {
		return nil, false
	}
	return b.meta, true
}

// commitVersionLocked 提交 volVersion/usage 增量（调用方持 s.mu，prev 为覆盖写前条目）。
func (s *SecretdataFS) commitVersionLocked(next, newSize int64, prev *metaEntry) {
	s.volVersion = next
	s.usage += newSize
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
}

// uploadBlob 上传数据/元数据字节到底层（mtime 打散由调用方给定 mt）。
func (s *SecretdataFS) uploadBlob(ctx context.Context, p string, data []byte, mt int64) error {
	return s.inner.WriteFile(ctx, p, bytes.NewReader(data), int64(len(data)), mt)
}

// encryptContentSingle 把整文件内容按单分块策略加密，返回 EncryptionResult 与该分块
// 字节（去重池 blob 载体；单分块 OrigSHA256 == 整文件内容哈希）。
func (s *SecretdataFS) encryptContentSingle(data []byte, rel string) (*shardseal.EncryptionResult, []byte, error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("secretdata: 空内容不可去重")
	}
	tmp, err := os.MkdirTemp(s.temp, "chunks-dedup-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	src := filepath.Join(tmp, sanitizeName(rel))
	if werr := os.WriteFile(src, data, 0o600); werr != nil {
		return nil, nil, werr
	}
	bmin := int64(16)
	if int64(len(data)) < bmin {
		bmin = int64(len(data))
	}
	policy := shardseal.BlockPolicy{Mode: "random", Min: int64(len(data)), Max: int64(len(data)), BlockletMin: bmin, BlockletMax: int64(len(data))}
	out, err := shardseal.EncryptShards(src, tmp, s.secret, policy, s.metaPadTarget(), s.algoVer)
	if err != nil {
		return nil, nil, fmt.Errorf("secretdata: 去重单分块加密失败: %w", err)
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

// GC 扫描卷内容器：物理清理墓碑（Deleted=true meta）条目与其分块、以及无引用的孤立
// 分块 blob，返回删除的文件数。并发安全：仅在锁内短临界区快照容器/池，物理删除在
// 锁外执行（阻塞写路径的长时间扫描）。
func (s *SecretdataFS) GC(ctx context.Context) (int, error) {
	containers := s.gcContainers()
	poolRefs := s.gcPoolRefs()
	referenced := map[string]struct{}{}
	kills := []gcKill{}
	for _, c := range containers {
		referenced, kills = s.gcScanContainer(ctx, c, poolRefs, referenced, kills)
	}
	for _, k := range kills {
		_ = s.inner.Delete(ctx, path.Join(k.container, k.name))
	}
	return len(kills), nil
}

// gcContainers 快照全部容器（dirSegs 值 + 去重池容器）。
func (s *SecretdataFS) gcContainers() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// gcPoolRefs 快照去重池存活 blob 名（引用计数 >0）。
func (s *SecretdataFS) gcPoolRefs() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]struct{}{}
	for _, b := range s.dedupPool {
		if b.refs > 0 {
			out[b.name] = struct{}{}
		}
	}
	return out
}

// gcScanContainer 扫描单个容器：文件 meta 判定存活/墓碑，dir meta 与存活分块入引用，
// 未引用分块（含墓碑条目分块、孤立）列入删除计划。
func (s *SecretdataFS) gcScanContainer(ctx context.Context, c string, poolRefs, referenced map[string]struct{}, kills []gcKill) (map[string]struct{}, []gcKill) {
	entries, err := s.inner.ListDir(ctx, c)
	if err != nil {
		return referenced, kills
	}
	for _, f := range entries {
		if f.IsDir {
			continue
		}
		switch shardseal.ClassifyName(f.Name) {
		case shardseal.KindFileMeta:
			referenced, kills = s.gcScanFileMeta(ctx, c, f.Name, referenced, kills)
		case shardseal.KindDirMeta:
			referenced[f.Name] = struct{}{}
		default:
			if _, ok := referenced[f.Name]; !ok {
				if _, isPool := poolRefs[f.Name]; !isPool {
					kills = append(kills, kill(c, f.Name))
				}
			}
		}
	}
	return referenced, kills
}

// gcScanFileMeta 解析单个文件 meta：存活条目其分块+自身入环引用（不删）；墓碑条目其
// 分块与自身列入删除计划。
func (s *SecretdataFS) gcScanFileMeta(ctx context.Context, container, name string, referenced map[string]struct{}, kills []gcKill) (map[string]struct{}, []gcKill) {
	blob, rerr := readBlob(ctx, s.inner, path.Join(container, name))
	if rerr != nil {
		return referenced, kills
	}
	mm, merr := s.decryptFileMeta(blob)
	if merr != nil {
		return referenced, kills
	}
	if mm.Deleted {
		kills = append(kills, gcKill{container, name})
		for _, ci := range mm.Chunks {
			kills = append(kills, gcKill{container, ci.FileName})
		}
		return referenced, kills
	}
	referenced[name] = struct{}{}
	for _, ci := range mm.Chunks {
		referenced[ci.FileName] = struct{}{}
	}
	return referenced, kills
}

// kill 构造删除操作（辅助拼接，避免 map 字面量重复）。
func kill(container, name string) gcKill { return gcKill{container: container, name: name} }

// startGC 后台周期 GC：独立 goroutine + time.Ticker，不在写路径持锁扫描。
// 仅 opts.GCInterval>0 时由 NewFS 启动；ctx 取消即退出。
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
