// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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

// CurrentVersion 返回卷当前乐观锁版本。**乐观锁标注（方案 A）**：多进程**预留 API**
// （WriteFileIfVersion/DeleteIfVersion 的外部 expected 基版本）；单进程写路径内建版本
// （writeFile 落盘 meta.BaseVersion），本方法仅向预留 CAS 暴露当前版本。
func (s *SecretdataFS) CurrentVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.volVersion
}

// writeFileDedup 去重写路径（**实验性代码，未生产验证**——方案 A：Dedup 降级为预留，
// 装配层不接线，生产不可达；本实现保留供未来独立内容寻址子系统承接，测试框住正确性）。
// 整文件内容哈希查询卷级池 → 命中引用（不重复加密/上传），未命中首次加密并登记池。
// 数据分块恒在卷级 dedupDir，文件 meta blob 在自身 container。失败统一回收本次新建容器
// （created）。池命中判定与引用预留同持锁（I-3）。
func (s *SecretdataFS) writeFileDedup(ctx context.Context, wc writeCtx) (err error) {
	defer func() {
		if err != nil {
			s.pruneCreatedDirs(ctx, wc.created)
		}
	}()
	key := shardseal.Hash256(wc.data) // 整文件内容完整 SHA-256（256-bit，== 单分块 Chunks[0].OrigSHA256，重启后可重建）
	dir, err := s.ensureDedupDir()
	if err != nil {
		return err
	}
	mt := s.blobMTime(wc.mtime)
	s.mu.Lock()
	b, ok := s.dedupPool[key]
	if !ok || b.meta == nil {
		s.mu.Unlock()
		return s.writeFileDedupMiss(ctx, wc, key, dir, mt)
	}
	b.refs++ // 原子预留引用（锁内）：上传/提交失败再回滚，防并发 last-ref 删后悬空
	tpl := b.meta
	s.mu.Unlock()
	return s.writeFileDedupHit(ctx, wc, key, dir, mt, tpl)
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
// 改名后加密 meta 上传；失败/冲突回滚预留引用。BaseVersion 以本次写路径分配的 newVer
// 覆盖（Imp-1：去重引用条目同样落盘乐观锁版本，重启恢复卷版本不回退）。
func (s *SecretdataFS) writeFileDedupHit(ctx context.Context, wc writeCtx, key, dir string, mt int64, tpl *shardseal.Meta) error {
	ref := cloneMeta(tpl)
	ref.Original.Name = path.Base(wc.rel)
	ref.Original.MTime = mtimeString(wc.mtime)
	ref.BaseVersion = wc.newVer
	name, blob, err := s.encryptMetaBlob(ref)
	if err != nil {
		s.releasePoolRef(key)
		return err
	}
	metaPath := path.Join(wc.container, name)
	if werr := s.uploadBlob(ctx, metaPath, blob, mt); werr != nil {
		s.releasePoolRef(key)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	if cerr := s.commitDedupEntry(ctx, wc, &metaEntry{
		size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: name, meta: ref, dataDir: dir,
	}, []string{metaPath}); cerr != nil {
		s.releasePoolRef(key) // 提交失败/版本冲突：释放预留
		return cerr
	}
	return nil
}

// writeFileDedupMiss 池未命中：单分块加密，分块上传到池容器 + meta 上传到自身容器；
// 锁内登记池条目（refs=1）。并发未命中（同内容双首写）时保留各自私有分块，不覆盖他人。
func (s *SecretdataFS) writeFileDedupMiss(ctx context.Context, wc writeCtx, key, dir string, mt int64) error {
	o, chunkBytes, err := s.encryptContentSingle(wc.data, wc.rel)
	if err != nil {
		return err
	}
	chunkName := o.ChunkNames[0]
	chunkPath := path.Join(dir, chunkName)
	if werr := s.uploadBlob(ctx, chunkPath, chunkBytes, mt); werr != nil {
		return fmt.Errorf("secretdata: 上传去重分块失败: %w", werr)
	}
	meta := o.Meta
	meta.Original.Name = path.Base(wc.rel)
	meta.Original.MTime = mtimeString(wc.mtime)
	meta.BaseVersion = wc.newVer // Imp-1 修复：去重 miss 落盘 meta 也携带写路径分配版本
	if meta.Extra == nil {
		meta.Extra = map[string]any{}
	}
	meta.Extra["dedup"] = dir // string（Extra 统一 map[string]any，2026-10-07 用户裁定）
	name, metaBlob, err := s.encryptMetaBlob(meta)
	if err != nil {
		_ = s.inner.Delete(ctx, chunkPath)
		return err
	}
	metaPath := path.Join(wc.container, name)
	if werr := s.uploadBlob(ctx, metaPath, metaBlob, mt); werr != nil {
		_ = s.inner.Delete(ctx, chunkPath)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	s.mu.Lock()
	if wc.ifAbsent {
		// WriteIfAbsent：目标已存在（含本次写期间被并发者抢先提交）→ 清理本写上传、
		// 返回 errEntryExists（调用方映射为 (false,nil)），绝不覆盖他人条目。
		if _, ok := s.index[wc.rel]; ok {
			s.mu.Unlock()
			_ = s.inner.Delete(ctx, chunkPath)
			_ = s.inner.Delete(ctx, metaPath)
			return errEntryExists
		}
	}
	if wc.expected >= 0 && s.volVersion != wc.sv {
		s.mu.Unlock()
		_ = s.inner.Delete(ctx, chunkPath)
		_ = s.inner.Delete(ctx, metaPath)
		return versionConflictErr(wc.sv, wc.expected)
	}
	// 并发未命中（同 ID 双首写）：已有池条目则保留其 blob 作他人共享，本文件持自有
	// 分块（不覆盖 —— 覆盖会让他人 blob 丢引用）。entry 引用分块仍在本容器 dataDir。
	if _, exists := s.dedupPool[key]; !exists {
		s.dedupPool[key] = &dedupBlob{name: chunkName, refs: 1, meta: meta}
	}
	prev := s.index[wc.rel]
	s.index[wc.rel] = &metaEntry{size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: name, meta: meta, dataDir: dir, baseVersion: wc.newVer}
	addDirKeysLocked(s.dirs, wc.rel)
	s.volVersion = wc.newVer
	s.usage += int64(len(wc.data))
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	s.mu.Unlock()
	if prev != nil {
		// 同 commitEntry：锁外 best-effort 删旧版本（M-8 并发语义——索引已切新、旧分块
		// 无引用；并发在途读旧条目可能读到已删分块而报错，属可接受读-写语义，非损坏）。
		s.removeVersionMeta(prev)
	}
	return nil
}

// commitDedupEntry 提交去重条目：持锁 CAS（expected ≥0 须 volVersion==sv），以写路径
// 分配的 newVer 定型版本（Imp-1：与落盘 meta.BaseVersion 一致，不再锁内 +1 重算）。
// 失败清理本次上传路径并返回版本冲突错误（调用方负责回滚池预留引用）。
func (s *SecretdataFS) commitDedupEntry(ctx context.Context, wc writeCtx, e *metaEntry, cleanPaths []string) error {
	s.mu.Lock()
	if wc.ifAbsent {
		// WriteIfAbsent：目标已存在（含本次写期间被并发者抢先提交）→ 清理本写上传、
		// 返回 errEntryExists（调用方映射为 (false,nil)），绝不覆盖他人条目。
		if _, ok := s.index[wc.rel]; ok {
			s.mu.Unlock()
			for _, p := range cleanPaths {
				_ = s.inner.Delete(ctx, p)
			}
			return errEntryExists
		}
	}
	if wc.expected >= 0 && s.volVersion != wc.sv {
		s.mu.Unlock()
		for _, p := range cleanPaths {
			_ = s.inner.Delete(ctx, p)
		}
		return versionConflictErr(wc.sv, wc.expected)
	}
	prev := s.index[wc.rel]
	s.index[wc.rel] = e
	e.baseVersion = wc.newVer
	addDirKeysLocked(s.dirs, wc.rel)
	s.volVersion = wc.newVer
	s.usage += int64(len(wc.data))
	if prev != nil {
		s.usage -= prev.size
		if s.usage < 0 {
			s.usage = 0
		}
	}
	s.mu.Unlock()
	if prev != nil {
		// 同 commitEntry：锁外 best-effort 删旧版本（M-8 并发语义——索引已切新、旧分块
		// 无引用；并发在途读旧条目可能读到已删分块而报错，属可接受读-写语义，非损坏）。
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
	bmin := min(int64(len(data)), int64(16))
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
// 用于去重引用/自构造 meta 落盘（EncryptShards 之外的自构造 meta 也走同格式）。
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
	// 新命名：首/末段 = 同一加密 meta blob 的不同窗口（offset 0/16）base62，自包含可
	// 不解密验证密文完整；中段 = HMAC 分组盲签（同文件共享；无明文哈希外泄）。
	// 组签输入 = meta.Original.SHA256 解码为 raw 32 字节（与 shardseal 内部 GroupSig(secret, fullSum)
	// 一致——此前直接用 hex 字符串作 HMAC 输入，导致 chunk 组签（raw32）与 meta 组签（hex64）
	// 不一致，破坏无 meta 盲分组恢复）。
	fullSHA, derr := hex.DecodeString(m.Original.SHA256)
	if derr != nil || len(fullSHA) != 32 {
		return "", nil, fmt.Errorf("secretdata: meta 原文 SHA256 非法（须 64 hex）: %w", derr)
	}
	encA, encB := shardseal.Hash48Pair(blob, 0, 16)
	group := shardseal.GroupSig(s.secret, fullSHA)
	metaName, nerr := shardseal.MetaName(encA, group, encB)
	if nerr != nil {
		return "", nil, nerr
	}
	return metaName, blob, nil
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

// GC 清理磁盘孤儿分块与墓碑 meta（**可选维护工具，方案 A 降级**）：默认不启动（GCInterval
// 默认 0），仅在远程卷/多进程共享场景由调用方显式 fs.GC() 或配置 gc_interval>0 时启用，
// 作为孤儿兜底——**不再作为默认正确性依赖**（Delete 即时物理删保证删即释放；覆盖写/去重
// 失败路径即时回滚删新留旧）。返回删除文件数。
//
// 两阶段（先标记存活引用、再扫未引用分块）消除「分块在 file meta 之前被遍历到→误删」的
// 排序依赖（C-1）。
//
// **锁粒度（Imp-3 降级）**：标记阶段持 **RLock**（复用内存索引收集存活引用，零磁盘读、
// 零 scrypt），清扫阶段**按容器分批短持写锁**（每容器 Lock → 检查在途写 → 确认/清扫 →
// 删除该容器 kills → Unlock，再处理下一容器）——不再全程持整卷写锁扫全部容器，
// 大卷一轮 GC 对单容器的阻塞降为容器级（非全卷）。分段间新写出现：任一容器 Lock 时
// 发现 writesInFlight>0 即放弃本轮剩余容器（已删的确认墓碑/孤儿安全——它们无 meta
// 引用，不可能被在途写重新引用）。
//
// **已知窗口（方案 A 可接受，不修）**：后台 GC 的 epoch 竞态——「阶段 2 扫过容器 C 后、
// 阶段 3 锁 C 前已完整提交」的写可能被误判孤儿。因 GC 默认禁用、作为可选工具可接受
// 已知窗口；单实例即时删路径无此依赖。
//
// 标记阶段：遍历 s.index 收集存活 meta/分块/parity 引用 + 磁盘扫描只做第二遍孤儿判定。
// 挂载态 s.index 是存活 meta 的完整解密镜像（writesInFlight 门禁已排干在途写），故标记
// 直接遍历内存、零 scrypt；磁盘扫描只做孤儿判定。
//
// 复审修正（final-review-2 第 2 轮）：loadContainerFileMeta 对挂载期逐文件读/解密
// 失败**静默跳过**（非「不存在」），存活文件可能不在 s.index——若 GC 仅凭「不在内存索引」
// 删除其 meta/分块即**永久数据丢失**（瞬时故障 → GC 误删）。故对「不在 referenced 的
// KindFileMeta」（潜在待删集合，通常极小 = 仅挂载故障/孤儿）**磁盘重确认**：
//   - 重读+解密成功且非墓碑 → 存活文件（挂载故障被跳过）→ 其 meta+分块+parity 补入
//     referenced 保护，不删；
//   - 重读+解密成功且 Deleted → 确认墓碑 → 删其 meta（分块作孤儿判删）；
//   - 读/解密失败 → 无法确认死活 → 不删（fail-closed），且该容器本轮不清分块（其分块
//     可能被未决 meta 引用，误删即数据丢失）。
//
// 重确认只发生在「不在索引」的 meta 上，活跃文件（在索引）零磁盘读——O(1) 主目标保持。
func (s *SecretdataFS) GC(ctx context.Context) (int, error) {
	// 阶段 0/1：RLock 快照——在途写检查 + 容器/池引用 + 标记内存索引存活引用。
	// 不持写锁（标记纯读内存，零磁盘读/零 scrypt），大卷不阻塞写路径。
	s.mu.RLock()
	if s.writesInFlight > 0 {
		s.mu.RUnlock()
		return 0, nil // 在途写存在：放弃本轮，避免删未提交分块
	}
	containers := s.gcContainersLocked()
	poolRefs := s.gcPoolNamesLocked()
	// 存活引用 key: container + "/" + name（meta blob 与分块均以自包含容器定位）。
	referenced := map[string]struct{}{}
	s.gcMarkIndex(ctx, referenced)
	s.mu.RUnlock()

	// 阶段 2：按容器分批短持锁——磁盘重确认「不在索引」的潜在待删 meta（Imp-3 复审修正）
	// + 删除确认墓碑。unconfirmed：任一容器存在读/解密失败、无法判定死活的 meta →
	// 本轮全局不清分块（fail-closed 防误删其可能引用的分块；确认的墓碑 meta 仍删）。
	unconfirmed, deleted := s.gcReconfirmContainers(ctx, containers, referenced)
	// 阶段 3：无未确认 meta 时按容器分批清扫孤儿分块（墓碑分块无 meta 引用归属此层删）。
	if !unconfirmed {
		deleted += s.gcSweepContainers(ctx, containers, referenced, poolRefs)
	}
	return deleted, nil
}

// gcReconfirmContainers 阶段 2：逐容器短持锁重确认「不在索引」meta + 删确认墓碑
// （Imp-3 锁粒度降级：分段持锁，任一容器发现新写出现即放弃本轮剩余）。
// 返回 (是否存在未确认 meta, 已删文件数)。
//
// **取舍明示（修复轮 M3）**：重确认在容器级写锁内做磁盘读 + scrypt 派生（真实内存随 KDF
// 档位 high≈128MiB / standard≈16MiB / low≈4MiB，RFC 7914 = 128×r×N）——对「不在索引」的
// meta（healthy 卷为零、仅挂载故障/孤儿时非零）若几十个同时失联，本轮 GC 将在全局写锁下
// 做几十次 scrypt，整卷阻塞分钟级。GC 是可选维护工具、默认禁用，此代价可接受；但不要把
// 运行中的 GC 误判为「在线轻量扫描」。
func (s *SecretdataFS) gcReconfirmContainers(ctx context.Context, containers []string, referenced map[string]struct{}) (bool, int) {
	unconfirmed := false
	deleted := 0
	for _, c := range containers {
		s.mu.Lock()
		if s.writesInFlight > 0 {
			s.mu.Unlock()
			return unconfirmed, deleted // 分段间隙新写出现：放弃本轮剩余容器（已删确认墓碑安全）
		}
		kills := []gcKill{}
		if !s.gcReconfirmContainer(ctx, c, referenced, &kills) {
			unconfirmed = true
		}
		for _, k := range kills {
			if s.inner.Delete(ctx, path.Join(k.container, k.name)) == nil {
				deleted++
			}
		}
		s.mu.Unlock()
	}
	return unconfirmed, deleted
}

// gcSweepContainers 阶段 3：无未确认 meta 时逐容器短持锁清扫孤儿分块并删除，返回已删数。
func (s *SecretdataFS) gcSweepContainers(ctx context.Context, containers []string, referenced, poolRefs map[string]struct{}) int {
	deleted := 0
	for _, c := range containers {
		s.mu.Lock()
		if s.writesInFlight > 0 {
			s.mu.Unlock()
			return deleted // 新写出现：停止清扫剩余容器
		}
		kills := []gcKill{}
		s.gcSweepOrphanChunks(ctx, c, referenced, poolRefs, &kills)
		for _, k := range kills {
			if s.inner.Delete(ctx, path.Join(k.container, k.name)) == nil {
				deleted++
			}
		}
		s.mu.Unlock()
	}
	return deleted
}

// gcMarkIndex 从内存索引直接标记存活引用（Imp-3 核心）：遍历 s.index 全部活跃条目，
// 把其 meta blob（dirSeg/metaName）、全部分块（dataDir 或 dirSeg）与 parity blob 登记到
// referenced。s.index 在调用方持锁（RLock 亦可，纯读）下是完整镜像（与磁盘一致）；
// 墓碑/旧版本 meta 名登记「需删除」路径（惰性：扫描时按未引用判删）。
// 调用方持 s.mu（RLock 或 Lock 均可——纯读）。
func (s *SecretdataFS) gcMarkIndex(ctx context.Context, referenced map[string]struct{}) {
	for _, e := range s.index {
		if e == nil || e.meta == nil {
			continue
		}
		// meta blob 自身存活引用（在 dirSeg）；墓碑（Deleted）条目自索引清理时已删，
		// 不在 index —— 旧墓碑由 gcReconfirmContainer 重读确认后删。
		isDeleted := e.meta.Deleted
		// 数据分块所在容器：去重引用文件用 dataDir（Extra["dedup"]），否则 dirSeg。
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
		// 存活文件 meta blob 自身加入引用（防 gcReconfirmContainer 重读 + 保护重复登记；
		// 墓碑不登记——交由重确认删）。
		if !isDeleted {
			referenced[e.dirSeg+"/"+e.metaName] = struct{}{}
		}
	}
}

// gcReconfirmContainer 磁盘重确认容器内「不在内存索引」的潜在待删文件 meta（Imp-3 复审）。
// 对每个未入 referenced 的 KindFileMeta：重读+解密——存活 → 保护（meta+分块+parity 补入
// referenced）；墓碑 → 其 meta 列入删除；读/解密失败 → 返回 false（调用方本轮全局停扫
// 分块，防未决 meta 引用的分块被误删）。目录 meta 恒保留。调用方持 s.mu.Lock。
func (s *SecretdataFS) gcReconfirmContainer(ctx context.Context, c string, referenced map[string]struct{}, kills *[]gcKill) bool {
	entries, err := s.inner.ListDir(ctx, c)
	if err != nil {
		// 容器列表失败：容器内可能存在未确认存活的 meta（其分块可能被误扫为孤儿），
		// fail-closed → 全局停扫分块。
		return false
	}
	ok := true
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		if _, inIdx := referenced[c+"/"+f.Name]; inIdx {
			continue // 已在内存索引（挂载期解密成功）→ 无需重确认
		}
		mm, merr := s.gcConfirmFileMeta(ctx, c, f.Name)
		if merr != nil {
			// 读/解密失败：无法判定死活（可能是瞬时故障仍在、或损坏）→ 不删 + 停扫分块。
			ok = false
			continue
		}
		if mm.Deleted {
			// 确认墓碑：删其 meta blob（分块无 meta 引用，作孤儿判删）。
			*kills = append(*kills, gcKill{c, f.Name})
			continue
		}
		// 存活文件（挂载故障被跳过）：meta blob + 全部分块 + parity 补入 referenced 保护。
		s.gcProtectLiveMeta(referenced, c, f.Name, mm)
	}
	return ok
}

// gcConfirmFileMeta 重读并解密单个文件 meta blob（挂载故障被跳过的存活 meta 识别用）。
// 返回 mm（解密成功）+ err（读/解密失败，调用方按 fail-closed 处理）。调用方持 s.mu。
func (s *SecretdataFS) gcConfirmFileMeta(ctx context.Context, container, name string) (*shardseal.Meta, error) {
	blob, rerr := readBlob(ctx, s.inner, path.Join(container, name))
	if rerr != nil {
		return nil, rerr
	}
	return s.decryptFileMeta(blob)
}

// gcProtectLiveMeta 把已解密**存活**（非墓碑）meta 的自身 blob + 全部分块 + parity 登记为
// 存活引用（供 gcMarkIndex / gcReconfirmContainer 共用）。dataDir 语义与 loadContainerFileMeta
// 一致：去重引用文件分块在 Extra["dedup"] 容器，否则在本容器。调用方持 s.mu.Lock。
func (s *SecretdataFS) gcProtectLiveMeta(referenced map[string]struct{}, container, name string, mm *shardseal.Meta) {
	referenced[container+"/"+name] = struct{}{}
	dir := container
	if len(mm.Extra) > 0 {
		if dd, ok := mm.Extra["dedup"]; ok {
			if s, sok := dd.(string); sok && s != "" {
				dir = s
			}
		}
	}
	for _, ci := range mm.Chunks {
		referenced[dir+"/"+ci.FileName] = struct{}{}
	}
	if p := mm.Parity; p != nil {
		referenced[dir+"/"+p.FileName] = struct{}{}
	}
}

// gcSweepOrphanChunks 第二遍：删未引用且非池引用的孤立分块（墓碑分块因未引用归属此层删）。
// 调用方保证本容器无未决 meta（gcReconfirmContainer 全 true）——否则其分块可能被未决
// meta 引用，不得当孤儿清。目录 meta 恒保留。调用方持 s.mu.Lock。
func (s *SecretdataFS) gcSweepOrphanChunks(ctx context.Context, c string, referenced, poolRefs map[string]struct{}, kills *[]gcKill) {
	entries, err := s.inner.ListDir(ctx, c)
	if err != nil {
		return
	}
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindChunk {
			continue
		}
		if _, ok := referenced[c+"/"+f.Name]; ok {
			continue
		}
		if _, isPool := poolRefs[f.Name]; isPool {
			continue
		}
		*kills = append(*kills, gcKill{c, f.Name})
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

// startGC 后台周期 GC（**可选维护工具**）：独立 goroutine + time.Ticker。仅
// opts.GCInterval>0 时由 NewFS 启动（默认 0=禁用，正确性不依赖 GC）；ctx 取消即退出
// （backend.Close 取消）。
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
