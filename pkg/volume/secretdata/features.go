// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	storagepkg "github.com/cocomhub/sproxy/pkg/storage"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"

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

// RefreshIndex 重新扫描底层（容器树 + 全部文件 meta）并**原子重建**内存索引（S5，2026-10-27）。
//
// 背景（设计文档 §6 裁定 B）：secretdata 的内存索引只在挂载时 `loadIndex` 一次——若卷被
// **本进程之外**修改（用户用网盘 App 直改、另一 sproxy 实例写入），幽灵条目/旧哈希会一直
// 存在到重启。本方法提供显式收敛入口（服务端 `sclient volume refresh` / 未来 Web UI 按钮）。
//
// 实现：在**全新空白镜像**上重跑 loadIndex 的扫描/解密（锁外），成功后一次换入
// （s.index/dirs/dirSegs/dirIDs/dirParents/dirMTime/usage/volVersion/dedupPool/dirPhysTotal）。
// 换入前清空块级派生缓存（块/窗口/磁盘 span/stage）——旧索引的 key 在新索引下可能指向
// 已删除/改名的对象，缓存与索引必须同批失效（删除三段式 ③）。失败则**保持旧索引不动**
// （fail-closed：瞬时底层故障不能把 vol 变成空视图，否则 GC 会把全部 meta 当孤儿清扫）。
func (s *SecretdataFS) RefreshIndex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// ① 清空派生缓存（key 空间随索引重建变化）。
	s.invalidateAllBlockCachesLocked()
	// ② 造全新镜像：map 初始化后走既有扫描路径（loadIndex 是增量化登记，不能直接复用）。
	//    用 cloneFS 结构保持扫描函数签名不变（s 还是自身，但先把状态归零）。
	// 简化实现：把状态重置后调用 loadIndex（它内部只做登记）——先备份旧状态以回滚。
	if err := s.reindexLocked(ctx); err != nil {
		return err
	}
	return nil
}

// reindexLocked 在持锁下重建索引：先状态归零，跑同一条扫描管线，任一失败回滚旧状态。
func (s *SecretdataFS) reindexLocked(ctx context.Context) error {
	// 保存旧状态（回滚用）。
	old := struct {
		index        map[string]*metaEntry
		dirs         map[string]struct{}
		dirSegs      map[string]string
		dirIDs       map[string]string
		dirParents   map[string]string
		dirMTime     map[string]int64
		usage        int64
		volVersion   int64
		dedupPool    map[string]*dedupBlob
		dirPhysTotal int64
	}{
		index: s.index, dirs: s.dirs, dirSegs: s.dirSegs, dirIDs: s.dirIDs,
		dirParents: s.dirParents, dirMTime: s.dirMTime, usage: s.usage,
		volVersion: s.volVersion, dedupPool: s.dedupPool, dirPhysTotal: s.dirPhysTotal,
	}
	// 归零后跑扫描管线（其结果登记进归零后的 map）。
	s.index = map[string]*metaEntry{}
	s.dirs = map[string]struct{}{}
	s.dirSegs = map[string]string{}
	s.dirIDs = map[string]string{}
	s.dirParents = map[string]string{}
	s.dirMTime = map[string]int64{}
	s.usage = 0
	s.volVersion = 0
	s.dedupPool = map[string]*dedupBlob{}
	s.dirPhysTotal = 0
	if err := s.loadIndex(ctx); err != nil {
		// 失败：回滚旧状态（绝不让瞬时故障把卷变成空视图）。
		s.index, s.dirs = old.index, old.dirs
		s.dirSegs, s.dirIDs = old.dirSegs, old.dirIDs
		s.dirParents, s.dirMTime = old.dirParents, old.dirMTime
		s.usage, s.volVersion = old.usage, old.volVersion
		s.dedupPool, s.dirPhysTotal = old.dedupPool, old.dirPhysTotal
		return fmt.Errorf("secretdata: 重建索引失败（已回滚旧索引）: %w", err)
	}
	return nil
}

// invalidateAllBlockCachesLocked 清空全部块/窗口/磁盘派生缓存（RefreshIndex 前调用；持锁）。
// 注意仅做**内存侧失效**——磁盘 span/stage 缓存是尽力而为设施（key 带内容寻址段，新索引
// 下同名可能读旧 blob；由 spanCache/stageDL 的 checksum 校验自愈兜底）。
func (s *SecretdataFS) invalidateAllBlockCachesLocked() {
	if s.blockCache != nil {
		s.blockCache.clear()
	}
	if s.spanCache != nil {
		s.spanCache.clearAll()
	}
	if s.stageDL != nil {
		s.stageDL.invalidateAll()
	}
}

// writeFileDedup 去重写路径（**实验性代码，未生产验证**——方案 A：Dedup 降级为预留，
// 装配层不接线，生产不可达；本实现保留供未来独立内容寻址子系统承接，测试框住正确性）。
// 整文件内容哈希查询卷级池 → 命中引用（不重复加密/上传），未命中首次加密并登记池。
// 数据分块恒在卷级 dedupDir，文件 meta blob 在自身 container。失败统一回收本次新建容器
// （created）。池命中判定与引用预留同持锁（I-3）。
func (s *SecretdataFS) writeFileDedup(ctx context.Context, wc writeCtx) (err error) {
	defer func() {
		if err != nil {
			// 回收本次新建的空容器：走**既有回滚机制**（rollbackWrite，uploaded=nil）——它用
			// `WithTimeout(WithoutCancel(ctx), rollbackTimeout)` 做清理 ⇒ 调用方 ctx 取消/超时时
			// 仍能回收（此前直接传 ctx ⇒ 取消场景 Delete/ListDir 立即失败，留空容器 + 目录 meta；
			// 与主写路径的回滚同口径，单一机制）。
			s.rollbackWrite(ctx, nil, wc.created)
		}
	}()
	key := shardseal.Hash256(wc.data) // 整文件内容完整 SHA-256（256-bit，== 单分块 Chunks[0].OrigSHA256，重启后可重建）
	dir, err := s.ensureDedupDir(ctx)
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
	s.retainPoolEntry(b.meta) // 原子预留引用（锁内，与 unrefPoolEntry 对称的单一实现）：
	// 上传/提交失败再回滚，防并发 last-ref 删后悬空
	tpl := b.meta
	s.mu.Unlock()
	return s.writeFileDedupHit(ctx, wc, key, dir, mt, tpl)
}

// dedupDirLogicalPath 是卷级去重容器的**固定逻辑路径**（用作确定性容器名的派生输入）。
// 它不是用户目录（无目录 meta，不进索引），仅用于派生一个稳定的物理容器名（设计 §9 第 8 项）。
const dedupDirLogicalPath = ".dedup"

// ensureDedupDir 确保卷级去重容器存在（懒创建一次），返回其容器名。
//
// 容器名**确定性派生**（2026-11-16 修复，设计 §9 第 8 项）：原为 shardseal.RandDirName()（随机）
// ⇒ 每次重启 / 索引缓存丢失都会新建一个去重容器、旧容器数据成孤儿（与 2026-11-06「重复容器」
// 事故同型）。确定性派生让「重复创建」天然幂等（同名复用）；s.dedupDir 仅内存字段、不持久化，
// **非确定性就无法跨重启找回同一容器**。
//
// 兼容：老版本的**随机名**去重容器（卷根下、无目录 meta 的目录）若存在 ⇒ 「采用」其名（不移名、
// 不丢弃）——池内 blob 的引用（meta.Extra["dedup"] 存的就是容器名）随之继续有效；改名会打断
// 已落 meta 的引用，故这里只采用不改名。新卷（无遗留容器）用确定性名。
func (s *SecretdataFS) ensureDedupDir(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedupDir != "" {
		return s.dedupDir, nil
	}
	want, werr := shardseal.DirNameFor(s.secret, dirNameOf(dedupDirLogicalPath))
	if werr != nil {
		return "", werr
	}
	// 确定性目录已存在 ⇒ 直接复用（重启/缓存丢失后仍命中同一容器）。
	if ent, serr := s.inner.Stat(ctx, want); serr == nil && ent != nil {
		s.dedupDir = want
		return want, nil
	}
	// 兼容既有随机名去重容器：卷根下、无目录 meta 的目录（去重池无 dir meta）。
	if legacy, lerr := s.legacyDedupDir(ctx, want); lerr == nil && legacy != "" {
		slog.Warn("secretdata: 采用既有随机名去重容器（老版本遗留）；如需规范名请手工迁移",
			"volume", s.opts.VolumeName, "container", legacy, "canonical", want)
		s.dedupDir = legacy
		return legacy, nil
	}
	s.dedupDir = want
	return want, nil
}

// legacyDedupDir 在卷根寻找「老版本随机名去重容器」：卷根下**没有目录 meta** 的目录。
// 去重池容器不含目录 meta（它是非逻辑目录）；嵌套布局下卷根只应有根容器（含 dir meta）。
// 返回除 canonical 之外的首个候选（无则空串）。
func (s *SecretdataFS) legacyDedupDir(ctx context.Context, canonical string) (string, error) {
	entries, err := s.inner.ListDir(ctx, "")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir || e.Name == canonical {
			continue
		}
		if hasDirMetaName(ctx, s.inner, e.Name) {
			continue // 逻辑目录容器（有 dir meta），不是去重池
		}
		return e.Name, nil
	}
	return "", nil
}

// hasDirMetaName 报告容器目录内是否含目录 meta blob（含则说明是逻辑目录容器）。
// 列举失败保守返回 true（不把无法确认的目录当去重池采用）。
func hasDirMetaName(ctx context.Context, inner syncpkg.FS, container string) bool {
	entries, err := inner.ListDir(ctx, container)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if !e.IsDir && shardseal.ClassifyName(e.Name) == shardseal.KindDirMeta {
			return true
		}
	}
	return false
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
	// **先登记后上传**（P2 修复，2026-10-19）：此前 meta 上传失败只 `releasePoolRef`、**不删**
	// 已落 meta ⇒ 内层「写成功但复核失败」时永久孤儿（与本轮其它上传点同口径，单一机制）。
	uploaded := []string{}
	if werr := s.uploadBlobTracked(ctx, &uploaded, metaPath, blob, mt); werr != nil {
		s.rollbackWrite(ctx, uploaded, nil)
		s.releasePoolRef(key)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	if cerr := s.commitDedupEntry(ctx, wc, &metaEntry{
		size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: name, meta: ref, dataDir: dir,
		phys: physBytesOf(ref, int64(len(blob)), true),
	}, uploaded); cerr != nil {
		s.releasePoolRef(key) // 提交失败/版本冲突：释放预留
		return cerr
	}
	return nil
}

// writeFileDedupMiss 池未命中：单分块加密，分块上传到池容器 + meta 上传到自身容器；
// 锁内登记池条目（refs=1）。并发未命中（同内容双首写）时保留各自私有分块，不覆盖他人。
func (s *SecretdataFS) writeFileDedupMiss(ctx context.Context, wc writeCtx, key, dir string, mt int64) error {
	// 本地 staging：单分块密文目录（自管记账，与主写路径同口径）——去重池 blob 先落本地再上传
	// （与常规分块同口径：经 uploadLocalChunk 复用调用方暂存路径）。
	tmp, cleanup, terr := s.dedupStagingDir(ctx, wc.rel, int64(len(wc.data)))
	if terr != nil {
		return terr
	}
	defer cleanup()
	// **先登记后上传**（P2 修复，2026-10-19）：池 blob 名含随机 salt/nonce（不复用）⇒ 内层
	// 「写成功但复核失败」返回错误时 blob 已落远端，必须能从回滚集合删除，否则**永久孤儿**
	// （与主写路径 uploadLocalChunk / 交错内存交付同口径）。
	uploaded := []string{}
	o, _, uerr := s.encryptAndUploadDedupChunk(ctx, wc, dir, tmp, mt, &uploaded)
	if uerr != nil {
		s.rollbackWrite(ctx, uploaded, nil)
		return uerr
	}
	chunkName := o.ChunkNames[0]
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
		s.rollbackWrite(ctx, uploaded, nil)
		return err
	}
	metaPath := path.Join(wc.container, name)
	// **先登记后上传**（同 uploadLocalChunk 口径，P2 修复 2026-10-19）：meta blob 名含随机
	// salt/padding（不复用）⇒ 内层「写成功但复核失败」时它已落远端，必须能从回滚集合删除
	// （否则永久孤儿）。登记后下方两处显式 `Delete(metaPath)` 由 rollbackWrite 统一承担
	// （单一机制，避免“同一清理写两遍”）。
	uploaded = append(uploaded, metaPath)
	if werr := s.uploadBlob(ctx, metaPath, metaBlob, mt); werr != nil {
		s.rollbackWrite(ctx, uploaded, nil)
		return fmt.Errorf("secretdata: 上传去重 meta 失败: %w", werr)
	}
	// **锁内提交走单一实现**（commitLocked，P2 收敛 2026-10-19）：池登记作为 preInstall 在
	// CAS 通过后、安装条目前执行（与原先内联块语义一致）。
	entry := &metaEntry{
		size: int64(len(wc.data)), mtime: wc.mtime, dirSeg: wc.container, metaName: name, meta: meta,
		dataDir: dir, baseVersion: wc.newVer,
		phys: physBytesOf(meta, int64(len(metaBlob)), true),
	}
	return s.commitLocked(ctx, wc, entry, int64(len(wc.data)), func() {
		// 并发未命中（同 ID 双首写）：已有池条目则保留其 blob 作他人共享，本文件持自有
		// 分块（不覆盖 —— 覆盖会让他人 blob 丢引用）。entry 引用分块仍在本容器 dataDir。
		if _, exists := s.dedupPool[key]; !exists {
			s.dedupPool[key] = &dedupBlob{name: chunkName, refs: 1, meta: meta, phys: contentPhysBytes(meta)}
		}
	}, func() { s.rollbackWrite(ctx, uploaded, nil) })
}

// commitDedupEntry 提交去重条目：持锁 CAS（expected ≥0 须 volVersion==sv），以写路径
// 分配的 newVer 定型版本（Imp-1：与落盘 meta.BaseVersion 一致，不再锁内 +1 重算）。
// 失败清理本次上传路径并返回版本冲突错误（调用方负责回滚池预留引用）。
// commitDedupEntry 去重路径的锁内提交（薄封装：走单一实现 commitLocked，P2 收敛 2026-10-19）。
func (s *SecretdataFS) commitDedupEntry(ctx context.Context, wc writeCtx, e *metaEntry, cleanPaths []string) error {
	return s.commitLocked(ctx, wc, e, int64(len(wc.data)), nil, func() {
		s.rollbackWrite(ctx, cleanPaths, nil)
	})
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
			_ = s.inner.Delete(gcCleanupCtx(), path.Join(s.dedupDir, b.name))
		}
		delete(s.dedupPool, key)
	}
}

// versionConflictErr 构造版本冲突错误（哨兵包装）。
func versionConflictErr(sv, expected int64) error {
	return fmt.Errorf("secretdata: 版本冲突：卷当前 %d，调用方期望 %d: %w", sv, expected, ErrVersionConflict)
}

// uploadBlobStagedDeferred 把**内存 blob**（meta）经「**暂存文件 + DeferVerify**」上传。
//
// 为什么必须这样（2026-11-17 实测）：baidupcs 的「延迟上传」是**双断言**——
// `UploadHints.DeferVerify` **且** ctx 带**暂存本地路径**（`putDeferredIfRequested`）。
// 内存直传只满足前者 ⇒ 仍走 `Put` 的即时复核 ⇒ `waitPutVisible` 每次 90s × `maxPutAttempts`
// 3 次 = **270s** ✗（实测 1 MB 写入百度卷总耗时 **300.5s**，其中 ~278s 全在此；同期数据分块
// 走暂存路径，仅 22s ✓）。
//
// 契约：本函数**只负责上传**，调用方**必须**把该 blob 登记进随后的**有界批量校验**
// （`iu.markVerify` + `verifyBatchAndRepair`）——延迟上传**不等于放弃校验** ✗。
// 语义与分块完全一致：不可见 ⇒ 待复核（成功）；真不一致 ⇒ 失败并由上层覆盖写重传。
func (s *SecretdataFS) uploadBlobStagedDeferred(ctx context.Context, rel, container, name string, data []byte, mt int64) error {
	dir, release, terr := s.newChunkTempDir(metaTempPrefix, rel)
	if terr != nil {
		// **本地 staging 不可用 ⇒ 回退内存直传**（旧行为，零回归）：功能正确性优先。
		// 该次写入的 meta 会落回 `Put` 的即时复核（可能慢，但**不失败** ✗）；
		// 与「本地 staging 不可用 ⇒ 内存交付」的既有降级链同口径。
		slog.Warn("secretdata: meta 暂存不可用 ⇒ 回退内存直传（meta 复核走即时校验）",
			"rel", rel, "error", terr)
		return s.uploadBlob(ctx, path.Join(container, name), data, mt)
	}
	defer release()
	tmp := filepath.Join(dir, "blob")
	if werr := os.WriteFile(tmp, data, 0o600); werr != nil {
		return fmt.Errorf("secretdata: 写 meta 暂存文件失败: %w", werr)
	}
	f, oerr := os.Open(tmp)
	if oerr != nil {
		return fmt.Errorf("secretdata: 打开 meta 暂存文件失败: %w", oerr)
	}
	defer f.Close()
	// 双断言：DeferVerify + 暂存路径；未装配延迟能力的后端自行忽略（零回归）。
	wctx := syncpkg.WithStagedLocalPath(ctx, tmp)
	wctx = syncpkg.WithUploadHints(wctx, &syncpkg.UploadHints{
		RandomContent: true,
		Size:          int64(len(data)),
		MD5:           md5HexOf(data),
		CRC32:         strconv.FormatUint(uint64(crc32.ChecksumIEEE(data)), 10),
		Path:          tmp,
		DeferVerify:   true,
	})
	return s.inner.WriteFile(wctx, storagepkg.LogicalKey{}, path.Join(container, name), f, int64(len(data)), mt)
}

// uploadBlob 上传数据/元数据字节到底层（mtime 打散由调用方给定 mt）。
// 2026-10-21：内存 blob **顺带算整块 md5/crc32** 经 UploadHints 交给内层（baidupcs
// 免整块重扫；其它后端忽略零回归）。meta 也走这里（随机 pad 内容，同样免扫收益）。
func (s *SecretdataFS) uploadBlob(ctx context.Context, p string, data []byte, mt int64) error {
	wctx := ctx
	if len(data) > 0 {
		wctx = syncpkg.WithUploadHints(ctx, &syncpkg.UploadHints{
			RandomContent: true,
			Size:          int64(len(data)),
			MD5:           md5HexOf(data),
			CRC32:         strconv.FormatUint(uint64(crc32.ChecksumIEEE(data)), 10),
			// **延迟校验**（与分块路径同口径，见 uploadLocalChunk 的 DeferVerify）：百度**新目录树**
			// 的远端可见性延迟 **5–10 分钟**（见 uploadLocalChunk 内既有注释）⇒ 上传后的即时
			// ETag/Stat 复核必然拿不到（实测 `远端 ETag=`）⇒ 硬重试 3 次后报 ErrTransient ⇒
			// **整个写入 500**（实测症状：`上传 meta 失败: … 上传后内容复核不匹配超过 3 次`）✗。
			// meta blob 的正确性由**读路径解密校验**与卷级巡检（`pkg/server/verify`）兜底，
			// 与分块路径的既有取舍一致。
			DeferVerify: true,
		})
	}
	if werr := s.inner.WriteFile(wctx, storagepkg.LogicalKey{}, p, bytes.NewReader(data), int64(len(data)), mt); werr != nil {
		return werr
	}
	// meta blob 上传成功后写**精简缓存**（解密出明文 JSON 再加密，无 padding）。
	// 放在这里是因为 uploadBlob 是全部写路径（流式/内存/dedup/copy/meta_provider）的
	// 唯一落盘点 —— 一处覆盖，避免每条路径各加钩子而漏掉。
	switch shardseal.ClassifyName(path.Base(p)) {
	case shardseal.KindFileMeta, shardseal.KindDirMeta:
		if plain, derr := s.decryptBlob(data); derr == nil {
			s.putCompactMeta(p, plain, int64(len(data)))
		}
	}
	return nil
}

// dedupStagingDir 为去重写路径准备本地临时目录（自管 staging 配额记账），返回释放函数
// （幂等：释放配额 + 删目录）。目录名确定性派生自逻辑 rel（设计 §9.2）。
func (s *SecretdataFS) dedupStagingDir(ctx context.Context, rel string, size int64) (string, func(), error) {
	release, rerr := s.stagingReserve(ctx, size)
	if rerr != nil {
		return "", nil, rerr
	}
	tmp, tmpRelease, terr := s.newChunkTempDir(dedupTempPrefix, rel)
	if terr != nil {
		release()
		return "", nil, fmt.Errorf("secretdata: 创建去重临时目录失败: %w", terr)
	}
	return tmp, func() { release(); tmpRelease() }, nil
}

// encryptAndUploadDedupChunk 把去重整文件内容按单分块加密（落 tmp）并上传到池容器 dir
// （经既有暂存复用机制交付本地路径 ⇒ 内层零二次拷贝），返回加密结果与池内底层路径。
// **先登记后上传**：目标路径在调用内层前追加到 *uploaded（同 uploadLocalChunk 口径），
// 供调用方在内层「写成功但复核失败」时回滚删除（池 blob 名随机不复用 ⇒ 否则永久孤儿）。
func (s *SecretdataFS) encryptAndUploadDedupChunk(ctx context.Context, wc writeCtx, dir, tmp string, mt int64, uploaded *[]string) (*shardseal.EncryptionResult, string, error) {
	o, chunkLocal, err := s.encryptContentSingle(wc.data, wc.rel, tmp)
	if err != nil {
		return nil, "", err
	}
	if werr := s.uploadLocalChunk(ctx, dir, o.ChunkNames[0], chunkLocal, mt, chunkHash{}, uploaded); werr != nil {
		return nil, "", fmt.Errorf("secretdata: 上传去重分块失败: %w", werr)
	}
	return o, path.Join(dir, o.ChunkNames[0]), nil
}

// encryptContentSingle 把整文件内容按单分块策略加密，分块写入调用方给定的 tmp 目录（调用方负责
// 临时目录生命周期与 staging 配额），返回 EncryptionResult 与该单块分块的**本地路径**
// （去重池 blob 载体 / parity 密文；单分块 OrigSHA256 == 整文件内容哈希）。
// 走 EncryptShardsBytes 内存变体（Imp-2 部分修复：不再写临时源 + 二次整读，峰值 1× 文件），
// 上传一律经 uploadLocalChunk 复用调用方暂存路径（不把整块读进内存再交 reader）。
func (s *SecretdataFS) encryptContentSingle(data []byte, rel, tmp string) (*shardseal.EncryptionResult, string, error) {
	if len(data) == 0 {
		return nil, "", fmt.Errorf("secretdata: 空内容不可单块加密")
	}
	bmin := min(int64(len(data)), int64(16))
	policy := shardseal.BlockPolicy{Mode: "random", Min: int64(len(data)), Max: int64(len(data)), BlockletMin: bmin, BlockletMax: int64(len(data))}
	// 注：这里产生的 meta 会被调用方**重新加密**（`encryptMetaBlob`，MTime/CTime/版本已定稿）
	// ⇒ padTarget 只影响一个**丢弃**的 meta；用确定性基准值（不引入随机）。
	out, err := shardseal.EncryptShardsBytes(data, sanitizeName(rel), tmp, s.secret, policy, s.metaPadTargetFor(nil), s.algoVer)
	if err != nil {
		return nil, "", fmt.Errorf("secretdata: 单块分块加密失败: %w", err)
	}
	return out, filepath.Join(tmp, out.ChunkNames[0]), nil
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
	blob, berr := shardseal.EncryptMetaJSON(key, salt, metaJSON, s.metaPadTargetFor(metaJSON), s.convergent)
	if berr != nil {
		return "", nil, berr
	}
	// 命名：首/末段 = 同一加密 meta blob 的不同窗口（offset 0/16）base62，自包含可
	// 不解密验证密文完整；中段 = HMAC 分组盲签（同文件共享；无明文哈希外泄）。
	// 组签输入 = meta.Original.SHA256 解码为 raw 32 字节（与 shardseal 内部 GroupSig(secret, fullSum)
	// 一致——此前直接用 hex 字符串作 HMAC 输入，导致 chunk 组签（raw32）与 meta 组签（hex64）
	// 不一致，破坏无 meta 盲分组恢复）。
	fullSHA, derr := hex.DecodeString(m.Original.SHA256)
	if derr != nil || len(fullSHA) != 32 {
		return "", nil, fmt.Errorf("secretdata: meta 原文 SHA256 非法（须 64 hex）: %w", derr)
	}
	group := shardseal.GroupSig(s.secret, fullSHA)
	metaName, nerr := metaNameFor(s.convergent, blob, group)
	if nerr != nil {
		return "", nil, nerr
	}
	return metaName, blob, nil
}

// dirMetaNameFor 选目录 meta 名：conv（v2 收敛）用确定性命名（同 blob ⇒ 同名），否则
// 传统随机段命名。
func dirMetaNameFor(conv bool, blob []byte, dirID string) (string, error) {
	if conv {
		return shardseal.DirMetaNameConv(blob, dirID)
	}
	blobA, blobB := shardseal.Hash48Pair(blob, 0, 16)
	return shardseal.DirMetaName(blobA, dirID, blobB)
}

// metaNameFor 选 file meta 名：conv（v2 收敛）用确定性命名（同 blob ⇒ 同名），否则传统命名。
func metaNameFor(conv bool, blob []byte, group string) (string, error) {
	if conv {
		return shardseal.MetaNameConv(blob, group)
	}
	encA, encB := shardseal.Hash48Pair(blob, 0, 16)
	return shardseal.MetaName(encA, group, encB)
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
		deleted += s.deleteKillsLocked(ctx, kills)
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
		deleted += s.deleteKillsLocked(ctx, kills)
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
	// 只按名字分类（ClassifyName）⇒ 走轻量列举，避免外部卷逐条补精确 size（批量 meta）。
	entries, err := s.listInner(ctx, c)
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
func (s *SecretdataFS) gcSweepOrphanChunks(ctx context.Context, c string, referenced, poolRefs map[string]struct{}, kills *[]gcKill) int {
	return s.gcSweepOrphanChunksCounted(ctx, c, referenced, poolRefs, kills)
}

// gcSweepOrphanChunksCounted 与 gcSweepOrphanChunks 等价，但**返回扫描到的分块总数**（供覆盖率自检）。
func (s *SecretdataFS) gcSweepOrphanChunksCounted(ctx context.Context, c string, referenced, poolRefs map[string]struct{}, kills *[]gcKill) int {
	seen := 0
	// 只按名字分类 ⇒ 轻量列举（不补精确 size，外部卷不触发批量 meta）。
	entries, err := s.listInner(ctx, c)
	if err != nil {
		return 0
	}
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindChunk {
			continue
		}
		seen++
		if _, ok := referenced[c+"/"+f.Name]; ok {
			continue
		}
		if _, isPool := poolRefs[f.Name]; isPool {
			continue
		}
		*kills = append(*kills, gcKill{c, f.Name})
	}
	return seen
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

// deleteKillsLocked 在**锁内**批量删除 GC 判定为可删的 blob（调用方持 s.mu）。删除走
// boundedCleanupCtx（P2 修复，2026-10-21）：此前直接用调用方 ctx（GC 的 ctx 常为
// long-lived，且不受取消影响）⇒ 远端卷上 Delete 挂起会**持整卷写锁无限等待**，与本文件
// 「锁内远端删除必须有界」的既有判据矛盾。返回成功删除数。
func (s *SecretdataFS) deleteKillsLocked(ctx context.Context, kills []gcKill) int {
	cctx, cancel := boundedCleanupCtx(ctx)
	defer cancel()
	deleted := 0
	for _, k := range kills {
		if s.inner.Delete(cctx, path.Join(k.container, k.name)) == nil {
			deleted++
		}
	}
	return deleted
}

// gcCleanupCtx 是 GC 非锁内清理（去重池孤儿）的界定 ctx：不受取消影响 + 有界。
func gcCleanupCtx() context.Context {
	c, _ := boundedCleanupCtx(context.Background())
	return c
}

// OrphanChunks 只读列出**孤儿分块**（未被任何 meta 引用、且非去重池引用），**不删除任何数据**。
//
// 为什么需要（用户 2026-11-18）：`GC()` 会直接删除；清理生产卷时希望**先出清单、人工逐条判断**。
// 本方法复用 GC 的**引用集合计算**（同一份逻辑，不造第二份判据），只是把「删除」换成「收集」。
//
// 安全：
//   - **在途写入存在时拒绝**（fail-closed）：否则可能把「已落盘但 meta 尚未提交」的块误判为孤儿；
//   - 只按名字分类（`KindChunk`），不补 size（外部卷避免批量 meta 调用）。
func (s *SecretdataFS) OrphanChunks(ctx context.Context) (syncpkg.OrphanReport, error) {
	s.mu.RLock()
	if s.writesInFlight > 0 {
		s.mu.RUnlock()
		return syncpkg.OrphanReport{}, fmt.Errorf("secretdata: 存在在途写入，拒绝孤儿审计（避免把未提交块误判为孤儿）")
	}
	containers := s.gcContainersLocked()
	poolRefs := s.gcPoolNamesLocked()
	referenced := map[string]struct{}{}
	s.gcMarkIndex(ctx, referenced) // 内存索引（可能不完整）
	s.mu.RUnlock()

	// **权威引用来源 = 真实目录树**（2026-11-19 生产事故修复）：
	// 生产卷按设计启动跳过全量扫描 ⇒ 内存索引可能为空 ⇒ 仅凭索引会把全部分块判为孤儿。
	// 这里逐容器重读并解密 file meta 来重建引用集合（与 GC 的 `gcReconfirmContainer` 同一判据）。
	stats, terr := s.collectTreeReferences(ctx, containers, referenced)
	if terr != nil {
		return syncpkg.OrphanReport{}, terr
	}
	if stats.fileMetas == 0 {
		return syncpkg.OrphanReport{}, fmt.Errorf(
			"%w: 未读到任何 file meta（容器 %d 个），无法建立权威引用集合", ErrOrphanAuditUnsafe, len(containers))
	}

	rep := syncpkg.OrphanReport{Containers: len(containers)}
	totalChunks := 0
	for _, c := range containers {
		var kills []gcKill
		n := s.gcSweepOrphanChunksCounted(ctx, c, referenced, poolRefs, &kills)
		totalChunks += n
		for _, k := range kills {
			rep.Chunks = append(rep.Chunks, syncpkg.OrphanBlob{Container: k.container, Name: k.name})
		}
	}
	// fail-closed 自检：引用集合不可信 ⇒ 拒绝返回清单（宁可不出，也不给错误清单）。
	if err := checkReferenceCoverage(stats.fileMetas, len(referenced), len(rep.Chunks), totalChunks); err != nil {
		return syncpkg.OrphanReport{}, err
	}
	return rep, nil
}

// ErrOrphanAuditUnsafe 表示引用集合不可信（索引未就绪/覆盖率异常）⇒ 拒绝出清单或执行 GC。
var ErrOrphanAuditUnsafe = errors.New("secretdata: 引用集合不可信，拒绝孤儿审计/GC")

// orphanAuditMaxRatio 是「孤儿 / 全部分块」的容忍上限：超过即判定引用集合不完整（而非真有大量孤儿）。
const orphanAuditMaxRatio = 0.30

// orphanAuditMinChunks 是启用「孤儿占比」判定的最小分块数（小卷不适用比例判据）。
const orphanAuditMinChunks = 20

// defaultOrphanAuditConcurrency 是孤儿审计的默认并发度（远端往返型任务，串行会超时）。
const defaultOrphanAuditConcurrency = 16

// checkReferenceCoverage 校验引用集合自洽：索引必须非空，且孤儿占比不得超阈值。
//
// 为什么（2026-11-19 实测事故）：`gcMarkIndex` 只从**内存索引**取引用；生产卷按设计**启动跳过全量扫描**，
// 索引未就绪时引用集合为空 ⇒ 全部 19,073 个分块都被判为「孤儿」。若据此删除即**清空整个卷** ✗。
// 因此这里 fail-closed：宁可不出清单，也不能给出错误清单。
func checkReferenceCoverage(fileMetas, refCount, orphanCount, totalChunks int) error {
	// 权威引用来自磁盘树：必须至少读到一个 file meta 才可能建立引用集合（调用方已先行检查）。
	if fileMetas == 0 {
		return fmt.Errorf("%w: 未读到任何 file meta，无法建立引用集合", ErrOrphanAuditUnsafe)
	}
	// 有存活文件时引用集合不得为空（否则说明登记逻辑失效）。
	if refCount == 0 {
		return fmt.Errorf("%w: 读到 %d 个 file meta 但引用集合为空", ErrOrphanAuditUnsafe, fileMetas)
	}
	if totalChunks == 0 {
		return nil
	}
	// 小卷（测试/新卷）孤儿占比天然可能很高，不做比例判定——只保留「索引非空」这条硬判据。
	if totalChunks < orphanAuditMinChunks {
		return nil
	}
	if float64(orphanCount)/float64(totalChunks) > orphanAuditMaxRatio {
		return fmt.Errorf("%w: 孤儿占比 %d/%d 超过 %.0f%%（疑似引用集合不完整）",
			ErrOrphanAuditUnsafe, orphanCount, totalChunks, orphanAuditMaxRatio*100)
	}
	return nil
}

// treeRefStats 是「按真实目录树重建引用集合」的统计（供审计自检与日志）。
type treeRefStats struct {
	// fileMetas 是读到的 file meta blob 总数（含墓碑）。
	fileMetas int
	// live 是存活文件数。
	live int
	// tombstones 是墓碑数（其分块无引用 ⇒ 可作孤儿判删）。
	tombstones int
}

// collectTreeReferences 遍历全部容器、重读并解密 file meta，把存活文件的
// meta blob + 分块 + parity 登记进 referenced（**与 GC 的 gcReconfirmContainer 同一判据**）。
//
// 为什么（2026-11-19 生产实测）：`gcMarkIndex` 只依赖内存索引；生产卷启动跳过全量扫描时索引为空
// ⇒ 19,073 个分块被 100% 误判为孤儿。差集审计必须以**磁盘真相**为准，索引只能作为加速。
//
// fail-closed：任一容器列举失败或任一 meta 读/解密失败 ⇒ 返回错误（不给出可能误导的清单）。
func (s *SecretdataFS) collectTreeReferences(
	ctx context.Context, containers []string, referenced map[string]struct{},
) (treeRefStats, error) {
	// **有界并发**（2026-11-19）：生产卷 895 容器 + 921 个 file meta ⇒ 串行要 1800+ 次远端往返
	// （>30 分钟，客户端必超时）。这里按容器并行（每容器列举 + 读 meta），默认并发 16。
	workers := s.orphanAuditConcurrency()
	if workers > len(containers) {
		workers = len(containers)
	}
	if workers <= 0 {
		workers = 1
	}

	var (
		mu   sync.Mutex
		st   treeRefStats
		muEq sync.Mutex
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}

	var wg sync.WaitGroup
	ch := make(chan string)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range ch {
				one, err := s.collectContainerReferences(ctx, c, referenced, &mu)
				if err != nil {
					fail(err)
					return
				}
				muEq.Lock()
				st.fileMetas += one.fileMetas
				st.live += one.live
				st.tombstones += one.tombstones
				muEq.Unlock()
			}
		}()
	}
feed:
	for _, c := range containers {
		select {
		case ch <- c:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if firstErr != nil {
		return treeRefStats{}, firstErr
	}
	return st, nil
}

// orphanAuditConcurrency 返回孤儿审计的并发度（默认 16；可经 Options 覆盖，<=0 视为默认）。
func (s *SecretdataFS) orphanAuditConcurrency() int {
	if n := s.opts.OrphanAuditConcurrency; n > 0 {
		return n
	}
	return defaultOrphanAuditConcurrency
}

// collectContainerReferences 处理单个容器：轻量列举 → 读/解密全部 file meta → 登记引用。
// 引用写入经 mu 串行化（map 非并发安全）。
func (s *SecretdataFS) collectContainerReferences(
	ctx context.Context, c string, referenced map[string]struct{}, mu *sync.Mutex,
) (treeRefStats, error) {
	var st treeRefStats
	entries, lerr := s.listInner(ctx, c)
	if lerr != nil {
		return st, fmt.Errorf("%w: 列举容器失败 %s: %v", ErrOrphanAuditUnsafe, c, lerr)
	}
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		st.fileMetas++
		mm, merr := s.gcConfirmFileMeta(ctx, c, f.Name)
		if merr != nil {
			return st, fmt.Errorf("%w: 读取/解密 file meta 失败 %s/%s: %v",
				ErrOrphanAuditUnsafe, c, f.Name, merr)
		}
		if mm.Deleted {
			st.tombstones++
			continue
		}
		st.live++
		mu.Lock()
		s.gcProtectLiveMeta(referenced, c, f.Name, mm)
		mu.Unlock()
	}
	return st, nil
}
