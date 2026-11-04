// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// inspect.go 实现 pkg/volume.StorageInspector（真实存储审查）。
//
// 设计：docs/designs/2026-11-04-secretdata-storage-inspection.md
//
// 布局事实（本文件是唯一解析处，禁止在 pkg/server / CLI 另写一份）：
//   - 逻辑目录 → 随机容器目录（子容器物理嵌套在父容器内；卷根容器唯一）；
//   - 容器内：1 个 dir-meta（名含 q 标记）、N 个 file-meta（名含 z 标记）、M 个内容分块
//     （无标记）；parity 与普通分块**命名同形** ⇒ 只能解 meta 判定；
//   - 判型只能用 shardseal.ClassifyName（只看随机段；hash 段允许出现 q/z）。
//
// 只解 meta（名称/尺寸/分块/parity 等元数据），**不读内容、不落地明文、不修改状态**。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"

	"log/slog"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// inspectMaxEntries 是单次反向解释的条目上限（防超大容器打爆响应/内存）。
const inspectMaxEntries = 50000

var _ volume.StorageInspector = (*SecretdataFS)(nil)

// Trace 返回逻辑路径 rel 的真实落点（容器链 + meta + 分块 + parity + 去重引用）。
func (s *SecretdataFS) Trace(ctx context.Context, rel string, mode volume.InspectMode) (*volume.PathTrace, error) {
	key, err := cleanLogicalRel(rel)
	if err != nil {
		return nil, err
	}
	if up, ok := s.uploadSnapshot(key); ok {
		return s.traceUploading(up), nil
	}
	// 在途上传优先：meta 尚未提交 ⇒ 索引里还没有该 rel，但用户需要看到「正在上传」，
	// 而不是「路径不存在」。
	if up, ok := s.uploadSnapshot(key); ok {
		return s.traceUploading(up), nil
	}
	snap := s.snapshotTrace(key)
	switch {
	case snap.isFile:
		return s.traceFile(ctx, key, snap.entry, snap.dedupRefs, mode), nil
	case snap.isDir:
		return s.traceDir(ctx, key, snap.container, snap.mtime, 0, mode)
	default:
		s.logIndexMiss(key)
		return nil, fmt.Errorf("secretdata: 逻辑路径不存在: %w", fs.ErrNotExist)
	}
}

// traceUploading 组装「正在上传」的落点视图。
func (s *SecretdataFS) traceUploading(up uploadProgress) *volume.PathTrace {
	return &volume.PathTrace{
		SchemaVersion: volume.SchemaVersion, Mode: "uploading", Rel: up.rel, Kind: "file",
		Size:       up.total,
		Containers: s.containerChainByPath(up.container),
		Upload: &volume.UploadProgress{
			Rel: up.rel, Container: up.container, Total: up.total,
			Committed: up.committed, Chunks: up.chunks, StartedAt: up.startedAt,
		},
	}
}

// containersForDir 返回逻辑目录 dirRel 的容器链（卷根 → 叶），零磁盘 IO（纯索引）。
func (s *SecretdataFS) containersForDir(dirRel string) []volume.ContainerRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rels := []string{""}
	if dirRel != "" {
		var stack []string
		for cur := dirRel; cur != ""; cur = s.dirParents[cur] {
			stack = append(stack, cur)
		}
		for _, rel := range slices.Backward(stack) {
			rels = append(rels, rel)
		}
	}
	out := make([]volume.ContainerRef, 0, len(rels))
	for _, rel := range rels {
		seg, ok := s.dirSegs[rel]
		if !ok {
			continue
		}
		out = append(out, volume.ContainerRef{RealPath: seg, LogicalDir: rel, DirID: s.dirIDs[rel]})
	}
	return out
}

// containerChainByPath 按**容器路径**逐层反查逻辑目录（索引未命中的层留空 LogicalDir）。
func (s *SecretdataFS) containerChainByPath(container string) []volume.ContainerRef {
	if container == "" {
		return nil
	}
	segs := strings.Split(container, "/")
	out := make([]volume.ContainerRef, 0, len(segs))
	prefix := ""
	for _, seg := range segs {
		if prefix == "" {
			prefix = seg
		} else {
			prefix = prefix + "/" + seg
		}
		ref := volume.ContainerRef{RealPath: prefix}
		if rel, ok := s.dirOfContainer(prefix); ok {
			ref.LogicalDir = rel
			s.mu.RLock()
			ref.DirID = s.dirIDs[rel]
			s.mu.RUnlock()
		}
		out = append(out, ref)
	}
	return out
}

// traceSnapshot 是一次索引快照（不持锁使用，避免 IO 落进临界区）。
type traceSnapshot struct {
	entry     metaEntry
	isFile    bool
	isDir     bool
	container string
	mtime     int64
	dedupRefs int
}

// snapshotTrace 在锁内取条目快照与去重引用计数。
func (s *SecretdataFS) snapshotTrace(key string) traceSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var snap traceSnapshot
	if e, ok := s.index[key]; ok && e != nil {
		snap.entry = *e
		snap.isFile = true
	}
	if c, ok := s.dirSegs[key]; ok {
		snap.container = c
		snap.isDir = true
	}
	if key == "" {
		snap.isDir = true
	}
	snap.mtime = s.dirMTime[key]
	snap.dedupRefs = s.dedupRefCountLocked(&snap.entry)
	return snap
}

// dedupRefCountLocked 统计共享同一去重 blob 的索引条目数（调用方持 s.mu）。
func (s *SecretdataFS) dedupRefCountLocked(e *metaEntry) int {
	if e == nil || e.dataDir == "" || e.meta == nil || len(e.meta.Chunks) == 0 {
		return 0
	}
	want := e.meta.Chunks[0].FileName
	n := 0
	for _, other := range s.index {
		if other != nil && other.dataDir == e.dataDir && other.meta != nil &&
			len(other.meta.Chunks) > 0 && other.meta.Chunks[0].FileName == want {
			n++
		}
	}
	return n
}

// logIndexMiss 记录索引未命中的规模（Debug；不含路径名，避免日志泄露用户内容名）。
func (s *SecretdataFS) logIndexMiss(key string) {
	s.mu.RLock()
	n, nd := len(s.index), len(s.dirSegs)
	s.mu.RUnlock()
	slog.Debug("secretdata: 审查索引未命中", "key_len", len(key), "index", n, "dir_segs", nd)
}

// traceFile 组装文件落点。
func (s *SecretdataFS) traceFile(ctx context.Context, rel string, e metaEntry, dedupRefs int, mode volume.InspectMode) *volume.PathTrace {
	tr := &volume.PathTrace{
		SchemaVersion: volume.SchemaVersion, Mode: mode.String(), Rel: rel, Kind: "file",
		Size: e.size, MTime: e.mtime,
		Containers: s.containersForDir(parentDirOf(rel)),
	}
	if e.meta == nil {
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: path.Join(e.dirSeg, e.metaName),
			Detail: "索引条目缺少 meta（可能未加载完成）",
		})
		return tr
	}
	if e.dataDir != "" {
		tr.Dedup = &volume.DedupRef{Container: e.dataDir, Refs: dedupRefs}
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalyDedupShared, RealPath: e.dataDir,
			Detail: "内容分块位于卷级去重容器（共享 blob）",
		})
	}
	metaBytes := metaBlobBytes(&e)
	meta := volume.BlobRef{RealPath: path.Join(e.dirSeg, e.metaName), Bytes: metaBytes}
	probeBlob(ctx, s, &meta, tr, mode)
	tr.Meta = &meta

	dataSeg := e.dataDir
	if dataSeg == "" {
		dataSeg = e.dirSeg
	}
	for i := range e.meta.Chunks {
		c := e.meta.Chunks[i]
		blob := volume.BlobRef{RealPath: path.Join(dataSeg, c.FileName), Bytes: c.EncSize, Plain: c.OrigSize}
		probeBlob(ctx, s, &blob, tr, mode)
		tr.Chunks = append(tr.Chunks, volume.ChunkRef{Index: c.Index, Blob: blob, Blocklets: len(c.Blocklets)})
	}
	if p := e.meta.Parity; p != nil {
		blob := volume.BlobRef{RealPath: path.Join(dataSeg, p.FileName), Bytes: p.EncSize, Plain: p.PlainLen}
		probeBlob(ctx, s, &blob, tr, mode)
		tr.Parity = &blob
	}
	return tr
}

// traceDir 组装目录落点（容器 + dir-meta blob）。
func (s *SecretdataFS) traceDir(ctx context.Context, rel, container string, mtime int64, _ int, mode volume.InspectMode) (*volume.PathTrace, error) {
	tr := &volume.PathTrace{
		SchemaVersion: volume.SchemaVersion, Mode: mode.String(), Rel: rel, Kind: "dir", MTime: mtime,
		Containers: s.containersForDir(rel),
	}
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读取容器 %q 失败: %w", container, err)
	}
	var dirMetas []string
	for _, en := range entries {
		if shardseal.ClassifyName(en.Name) == shardseal.KindDirMeta {
			dirMetas = append(dirMetas, en.Name)
		}
	}
	switch len(dirMetas) {
	case 0:
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: container,
			Detail: "容器内未找到 dir-meta blob",
		})
	case 1:
		for _, en := range entries {
			if en.Name != dirMetas[0] {
				continue
			}
			blob := volume.BlobRef{RealPath: path.Join(container, en.Name), Bytes: en.Size}
			probeBlob(ctx, s, &blob, tr, mode)
			tr.Meta = &blob
		}
	default:
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalyDirMetaAmbiguous, RealPath: container,
			Detail: fmt.Sprintf("容器内有 %d 个 dir-meta blob（异常布局）", len(dirMetas)),
		})
		for _, en := range entries {
			if en.Name != dirMetas[0] {
				continue
			}
			blob := volume.BlobRef{RealPath: path.Join(container, en.Name), Bytes: en.Size}
			probeBlob(ctx, s, &blob, tr, mode)
			tr.Meta = &blob
		}
	}
	return tr, nil
}

// Explain 解释真实路径的用途与逻辑归属。
func (s *SecretdataFS) Explain(ctx context.Context, realArg string, mode volume.InspectMode) (*volume.RealExplanation, error) {
	realPath, err := cleanRealPath(realArg)
	if err != nil {
		return nil, err
	}
	ex := &volume.RealExplanation{
		SchemaVersion: volume.SchemaVersion, Mode: mode.String(), RealPath: realPath,
	}
	// 注意 FS 契约：LocalFS 对「不存在」返回 (nil, nil)（不是 fs.ErrNotExist）；
	// 部分外部后端返回 (nil, fs.ErrNotExist)。两种都必须按「不存在」处置。
	ent, statErr := s.inner.Stat(ctx, realPath)
	if statErr == nil && ent == nil {
		statErr = fs.ErrNotExist
	}
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			ex.Kind = "unknown"
			ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
				Code: volume.AnomalyNotFound, RealPath: realPath,
				Detail: "该路径在真实存储上不存在",
			})
			return ex, nil
		}
		return nil, fmt.Errorf("secretdata: 读取真实路径 %q 失败: %w", realPath, statErr)
	}
	if ent != nil && ent.IsDir {
		return s.explainContainer(ctx, ex, realPath, mode)
	}
	return s.explainBlob(ctx, ex, realPath, mode)
}

// explainContainer 解释容器目录：本目录逻辑归属 + 逐条用途。
func (s *SecretdataFS) explainContainer(ctx context.Context, ex *volume.RealExplanation, realPath string, mode volume.InspectMode) (*volume.RealExplanation, error) {
	ex.Kind = "container"
	entries, err := s.inner.ListDir(ctx, realPath)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读取容器 %q 失败: %w", realPath, err)
	}
	entries = sortAndTruncateEntries(entries, ex, realPath)
	dirMetaName, dirMetaCount := findDirMeta(entries)
	recordContainerDirMetaState(ex, realPath, dirMetaName, dirMetaCount)
	if dirMetaName != "" {
		ex.Container, ex.Evidence = s.resolveContainer(ctx, realPath, dirMetaName, ex)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, en := range entries {
		seen[en.Name] = struct{}{}
		ex.Entries = append(ex.Entries, s.classifyRealEntry(ctx, realPath, en, dirMetaName, ex))
	}
	if mode == volume.InspectDisk {
		appendMissingBlobs(ex, realPath, s.containerExpectations(realPath), seen)
	}
	return ex, nil
}

// sortAndTruncateEntries 排序并截断超限条目（附 truncated 异常）。
func sortAndTruncateEntries(entries []syncpkg.Entry, ex *volume.RealExplanation, realPath string) []syncpkg.Entry {
	if len(entries) > inspectMaxEntries {
		entries = entries[:inspectMaxEntries]
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyTruncated, RealPath: realPath,
			Detail: fmt.Sprintf("条目超过 %d，已截断", inspectMaxEntries),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// findDirMeta 返回容器内首个 dir-meta 名与总数（正常恰好 1 个）。
func findDirMeta(entries []syncpkg.Entry) (name string, count int) {
	for _, en := range entries {
		if shardseal.ClassifyName(en.Name) != shardseal.KindDirMeta {
			continue
		}
		count++
		if name == "" {
			name = en.Name
		}
	}
	return name, count
}

// recordContainerDirMetaState 记录容器自身 dir-meta 缺失/重复异常。
func recordContainerDirMetaState(ex *volume.RealExplanation, realPath, dirMetaName string, dirMetaCount int) {
	switch {
	case dirMetaCount == 0:
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyContainerNotIndexed, RealPath: realPath,
			Detail: "容器内无 dir-meta（无法确定逻辑目录）",
		})
	case dirMetaCount > 1:
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyDirMetaAmbiguous, RealPath: realPath,
			Detail: fmt.Sprintf("容器内有 %d 个 dir-meta blob（异常布局）", dirMetaCount),
		})
	}
	_ = dirMetaName
}

// classifyRealEntry 判定容器内单个条目的用途与归属。
func (s *SecretdataFS) classifyRealEntry(ctx context.Context, container string, en syncpkg.Entry, dirMetaName string, ex *volume.RealExplanation) volume.RealEntry {
	// 目录条目 = **子容器**（逻辑目录），不是 blob：必须单独判定，否则会被误报为孤儿分块。
	if en.IsDir {
		return s.classifySubContainer(ctx, container, en)
	}
	switch shardseal.ClassifyName(en.Name) {
	case shardseal.KindDirMeta:
		return dirMetaEntry(en, dirMetaName, ex)
	case shardseal.KindFileMeta:
		return s.classifyFileMetaEntry(container, en, ex)
	default:
		return s.classifyChunkEntry(container, en, ex)
	}
}

// dirMetaEntry 判定 dir-meta 条目（本目录自身的加密 meta）。
func dirMetaEntry(en syncpkg.Entry, dirMetaName string, ex *volume.RealExplanation) volume.RealEntry {
	re := volume.RealEntry{Name: en.Name, Kind: "dir_meta", Bytes: en.Size, Note: "本目录的加密 meta"}
	if ex.Container != nil && en.Name == dirMetaName {
		re.Note = "本目录的加密 meta（dir_id=" + ex.Container.DirID + "）"
	}
	return re
}

func (s *SecretdataFS) classifyFileMetaEntry(container string, en syncpkg.Entry, ex *volume.RealExplanation) volume.RealEntry {
	re := volume.RealEntry{Name: en.Name, Kind: "file_meta", Bytes: en.Size}
	if owner := s.lookupFileByMetaName(container, en.Name); owner != nil {
		re.Owner = owner
		re.Note = "文件 meta（逻辑路径见 owner）"
		return re
	}
	re.Note = "文件 meta 但索引无对应条目（索引未加载/外部写入）"
	ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
		Code: volume.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
		Detail: "file-meta 无法反查到逻辑文件；疑似孤儿（删除请走 GC）",
	})
	return re
}

// classifyChunkEntry 判定内容分块/parity：索引命中 → 正常归属；
// 否则若属于**本进程在途上传** → upload_in_progress（不是孤儿）；再否则 → blob_orphan。
func (s *SecretdataFS) classifyChunkEntry(container string, en syncpkg.Entry, ex *volume.RealExplanation) volume.RealEntry {
	re := volume.RealEntry{Name: en.Name, Kind: "chunk", Bytes: en.Size}
	owner := s.lookupChunkOwner(container, en.Name)
	if owner != nil {
		if owner.Parity {
			re.Kind = "parity"
			re.Note = "XOR 奇偶校验段"
		} else {
			re.Note = "内容分块"
		}
		re.Owner = owner
		return re
	}
	if up, ok := s.uploadForContainer(container); ok {
		re.Kind = "upload_in_progress"
		re.Owner = &volume.LogicalRef{Rel: up.rel, Role: "file"}
		re.Note = "上传进行中（meta 尚未提交；提交后成为正式分块）"
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyUploadInProgress, RealPath: path.Join(container, en.Name),
			Detail: "属于本进程在途上传（rel=" + up.rel + "）；不是孤儿，请勿清理",
		})
		return re
	}
	re.Note = "内容分块/parity 但索引无引用；疑似孤儿（删除请走 GC）"
	ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
		Code: volume.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
		Detail: "分块无索引引用；疑似孤儿（删除请走 GC）",
	})
	return re
}

// classifySubContainer 判定子容器目录条目。
func (s *SecretdataFS) classifySubContainer(ctx context.Context, container string, en syncpkg.Entry) volume.RealEntry {
	re := volume.RealEntry{Name: en.Name, Kind: "container", Bytes: en.Size}
	child := path.Join(container, en.Name)
	if ref, ok := s.resolveChildContainer(ctx, child); ok {
		re.Container = ref
		re.Note = "子容器（逻辑目录 " + displayDirName(ref.LogicalDir) + "）"
	} else {
		re.Note = "子容器（dir-meta 未解出）"
	}
	return re
}

// uploadForContainer 返回写目标容器为 container 的在途上传（同容器多并发取首个）。
func (s *SecretdataFS) uploadForContainer(container string) (uploadProgress, bool) {
	ups := s.uploadsForContainer(container)
	if len(ups) == 0 {
		return uploadProgress{}, false
	}
	return ups[0], true
}

// resolveChildContainer 解子容器的 dir-meta，返回其逻辑目录归属。
func (s *SecretdataFS) resolveChildContainer(ctx context.Context, container string) (*volume.ContainerRef, bool) {
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return nil, false
	}
	for _, en := range entries {
		if shardseal.ClassifyName(en.Name) != shardseal.KindDirMeta {
			continue
		}
		blob, rerr := readBlob(ctx, s.inner, path.Join(container, en.Name))
		if rerr != nil {
			return nil, false
		}
		dm, derr := s.decryptDirMeta(blob)
		if derr != nil {
			return nil, false
		}
		ref := &volume.ContainerRef{RealPath: container, DirID: dm.DirID, ParentID: dm.ParentDirID}
		if rel, ok := s.resolveDirByID(dm.DirID); ok {
			ref.LogicalDir = rel
		}
		return ref, true
	}
	return nil, false
}

// displayDirName 渲染逻辑目录名（根 = "(卷根)"）。
func displayDirName(rel string) string {
	if rel == "" {
		return "(卷根)"
	}
	return rel
}

// appendMissingBlobs 在 disk 模式补充「索引声明但磁盘缺失」的差异码。
func appendMissingBlobs(ex *volume.RealExplanation, container string, expected, seen map[string]struct{}) {
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyBlobMissing, RealPath: path.Join(container, name),
			Detail: "索引声明该 blob 应存在，但磁盘上缺失",
		})
	}
}

// explainBlob 解释单个真实文件（dir-meta / file-meta / 分块 / parity）。
func (s *SecretdataFS) explainBlob(ctx context.Context, ex *volume.RealExplanation, realPath string, _ volume.InspectMode) (*volume.RealExplanation, error) {
	base := path.Base(realPath)
	container := path.Dir(realPath)
	if container == "." {
		container = ""
	}
	blob, err := readBlob(ctx, s.inner, realPath)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读取 blob %q 失败: %w", realPath, err)
	}
	switch shardseal.ClassifyName(base) {
	case shardseal.KindDirMeta:
		s.explainDirMetaBlob(ex, container, base, blob)
	case shardseal.KindFileMeta:
		s.explainFileMetaBlob(ex, container, base, realPath, blob)
	default:
		s.explainChunkBlob(ex, container, base, realPath)
	}
	return ex, nil
}

// explainDirMetaBlob 解释 dir-meta blob（容器逻辑归属）。
func (s *SecretdataFS) explainDirMetaBlob(ex *volume.RealExplanation, container, base string, blob []byte) {
	ex.Kind = "dir_meta"
	dm, derr := s.decryptDirMeta(blob)
	if derr != nil {
		_ = dm
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: path.Join(container, base), Detail: derr.Error(),
		})
		return
	}
	ex.Container, ex.Evidence = s.resolveContainer(context.Background(), container, base, ex)
}

// explainFileMetaBlob 解释 file-meta blob（解 meta + 索引反查归属）。
func (s *SecretdataFS) explainFileMetaBlob(ex *volume.RealExplanation, container, base, realPath string, blob []byte) {
	ex.Kind = "file_meta"
	mm, derr := s.decryptFileMeta(blob)
	if derr != nil {
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: realPath, Detail: derr.Error(),
		})
		return
	}
	ex.Evidence = append(ex.Evidence,
		fmt.Sprintf("meta Original.Name=%s size=%d chunks=%d", mm.Original.Name, mm.Original.Size, len(mm.Chunks)))
	if owner := s.lookupFileByMetaName(container, base); owner != nil {
		ex.Owner = owner
		ex.Evidence = append(ex.Evidence, "索引反查命中（容器 + meta 名）")
	} else {
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyBlobOrphan, RealPath: realPath,
			Detail: "meta 可解密但索引无对应逻辑文件（索引未加载/外部写入）；疑似孤儿（删除请走 GC）",
		})
	}
	if c, ok := s.containerOfDir(container); ok {
		ex.Container = c
	}
}

// explainChunkBlob 解释内容分块/parity（命中索引则给出归属，未命中记孤儿）。
func (s *SecretdataFS) explainChunkBlob(ex *volume.RealExplanation, container, base, realPath string) {
	ex.Kind = "chunk"
	owner := s.lookupChunkOwner(container, base)
	switch {
	case owner == nil:
		if up, ok := s.uploadForContainer(container); ok {
			ex.Kind = "upload_in_progress"
			ex.Owner = &volume.LogicalRef{Rel: up.rel, Role: "file"}
			ex.Evidence = append(ex.Evidence, "命中本进程在途上传注册表（meta 尚未提交）")
			ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
				Code: volume.AnomalyUploadInProgress, RealPath: realPath,
				Detail: "属于本进程在途上传（rel=" + up.rel + "）；不是孤儿，请勿清理",
			})
			if c, ok := s.containerOfDir(container); ok {
				ex.Container = c
			}
			return
		}
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyBlobOrphan, RealPath: realPath,
			Detail: "分块无索引引用；疑似孤儿（删除请走 GC）",
		})
		return
	case owner.Parity:
		ex.Kind = "parity"
	}
	ex.Owner = owner
	ex.Evidence = append(ex.Evidence, "索引反查命中（容器 + 分块名）")
	if c, ok := s.containerOfDir(container); ok {
		ex.Container = c
	}
}

// resolveContainer 解 dir-meta 并解析其逻辑目录（沿 dir_id/parent 链）。
func (s *SecretdataFS) resolveContainer(ctx context.Context, container, dirMetaName string, ex *volume.RealExplanation) (*volume.ContainerRef, []string) {
	ref := &volume.ContainerRef{RealPath: container}
	blob, err := readBlob(ctx, s.inner, path.Join(container, dirMetaName))
	if err != nil {
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: path.Join(container, dirMetaName), Detail: err.Error(),
		})
		return ref, nil
	}
	dm, derr := s.decryptDirMeta(blob)
	if derr != nil {
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: path.Join(container, dirMetaName), Detail: derr.Error(),
		})
		return ref, nil
	}
	ref.DirID = dm.DirID
	ref.ParentID = dm.ParentDirID
	rel, ok := s.resolveDirByID(dm.DirID)
	if !ok {
		ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
			Code: volume.AnomalyContainerNotIndexed, RealPath: container,
			Detail: "dir-meta 的 dir_id 在索引中不存在（索引未加载或外部创建）",
		})
		return ref, []string{"dir-meta 解密成功 dir_id=" + dm.DirID}
	}
	ref.LogicalDir = rel
	evidence := []string{"dir-meta 解密成功 dir_id=" + dm.DirID + " name=" + dm.Name}
	if dm.ParentDirID != "" {
		if _, parentOK := s.resolveDirByID(dm.ParentDirID); !parentOK {
			ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
				Code: volume.AnomalyParentUnresolved, RealPath: container,
				Detail: "dir-meta 的 parent_dir_id 在索引中缺失: " + dm.ParentDirID,
			})
		}
	}
	return ref, evidence
}

// containerExpectations 返回索引声明应位于该容器的 blob 名集合（disk 模式差异用）。
func (s *SecretdataFS) containerExpectations(container string) map[string]struct{} {
	out := make(map[string]struct{})
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.index {
		if e == nil || e.meta == nil {
			continue
		}
		if e.dirSeg == container {
			out[e.metaName] = struct{}{}
		}
		dataSeg := e.dataDir
		if dataSeg == "" {
			dataSeg = e.dirSeg
		}
		if dataSeg != container {
			continue
		}
		for _, c := range e.meta.Chunks {
			out[c.FileName] = struct{}{}
		}
		if p := e.meta.Parity; p != nil {
			out[p.FileName] = struct{}{}
		}
	}
	return out
}

// lookupFileByMetaName 反查「容器 + meta 名 → 逻辑文件」。
func (s *SecretdataFS) lookupFileByMetaName(container, metaName string) *volume.LogicalRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, e := range s.index {
		if e == nil || e.dirSeg != container || e.metaName != metaName {
			continue
		}
		return &volume.LogicalRef{Rel: rel, Role: "file"}
	}
	return nil
}

// lookupChunkOwner 反查「容器 + 分块名 → 逻辑文件 + 分块序号/parity」。
//
// parity 与普通分块**命名同形**：只能靠 meta.Parity.FileName 判定（不能按名字猜）。
func (s *SecretdataFS) lookupChunkOwner(container, name string) *volume.LogicalRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, e := range s.index {
		if owner, ok := chunkOwnerOfEntry(rel, e, container, name); ok {
			return owner
		}
	}
	return nil
}

// chunkOwnerOfEntry 判定单条目是否拥有该分块名（返回归属）。
func chunkOwnerOfEntry(rel string, e *metaEntry, container, name string) (*volume.LogicalRef, bool) {
	if e == nil || e.meta == nil {
		return nil, false
	}
	dataSeg := e.dataDir
	if dataSeg == "" {
		dataSeg = e.dirSeg
	}
	if dataSeg != container {
		return nil, false
	}
	if p := e.meta.Parity; p != nil && p.FileName == name {
		return &volume.LogicalRef{Rel: rel, Role: "file", Parity: true}, true
	}
	for i := range e.meta.Chunks {
		if e.meta.Chunks[i].FileName != name {
			continue
		}
		idx := e.meta.Chunks[i].Index
		if idx < 0 {
			idx = i
		}
		return &volume.LogicalRef{Rel: rel, Role: "file", ChunkIndex: &idx}, true
	}
	return nil, false
}

// containerOfDir 反查「容器 → 逻辑目录」。
func (s *SecretdataFS) containerOfDir(container string) (*volume.ContainerRef, bool) {
	rel, ok := s.dirOfContainer(container)
	if !ok {
		return nil, false
	}
	ref := &volume.ContainerRef{RealPath: container, LogicalDir: rel}
	s.mu.RLock()
	ref.DirID = s.dirIDs[rel]
	s.mu.RUnlock()
	return ref, true
}

// dirOfContainer 返回该容器对应的逻辑目录 rel。
func (s *SecretdataFS) dirOfContainer(container string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, seg := range s.dirSegs {
		if seg == container {
			return rel, true
		}
	}
	return "", false
}

// resolveDirByID 反查「dir_id → 逻辑目录 rel」。
func (s *SecretdataFS) resolveDirByID(id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, did := range s.dirIDs {
		if did == id {
			return rel, true
		}
	}
	return "", false
}

// metaBlobBytes 返回条目 meta blob 的落盘字节（phys 已含分块与 parity）。
func metaBlobBytes(e *metaEntry) int64 {
	if e == nil {
		return 0
	}
	if e.dataDir != "" {
		return e.phys // 去重引用条目：phys 只含自身 meta blob
	}
	if content := contentPhysBytes(e.meta); content > 0 && content <= e.phys {
		return e.phys - content
	}
	return e.phys
}

// probeBlob 在 disk 模式下探测 blob 存在性与大小，并写入差异码。
func probeBlob(ctx context.Context, s *SecretdataFS, ref *volume.BlobRef, tr *volume.PathTrace, mode volume.InspectMode) {
	if mode != volume.InspectDisk {
		return
	}
	ent, err := s.inner.Stat(ctx, ref.RealPath)
	if err == nil && ent == nil {
		err = fs.ErrNotExist // FS 契约：不存在可能返回 (nil, nil)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			present := false
			ref.OnDisk = &present
			tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
				Code: volume.AnomalyBlobMissing, RealPath: ref.RealPath,
				Detail: "索引/meta 声明该 blob，但磁盘上缺失",
			})
			return
		}
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalyMetaUnreadable, RealPath: ref.RealPath,
			Detail: "探测失败: " + err.Error(),
		})
		return
	}
	present := true
	ref.OnDisk = &present
	size := ent.Size
	ref.DiskBytes = &size
	if ref.Bytes > 0 && size != ref.Bytes {
		tr.Anomalies = append(tr.Anomalies, volume.Anomaly{
			Code: volume.AnomalySizeMismatch, RealPath: ref.RealPath,
			Detail: fmt.Sprintf("磁盘 %d 字节 ≠ 声明 %d 字节", size, ref.Bytes),
		})
	}
}

// cleanLogicalRel 规范化逻辑路径；含父级引用 ⇒ ErrOutsideVolume。
func cleanLogicalRel(rel string) (string, error) {
	p := strings.Trim(strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/")), "/")
	if p == "" {
		return "", nil
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", volume.ErrOutsideVolume
	}
	return c, nil
}

// cleanRealPath 规范化真实路径（卷根相对）：拒绝绝对路径与父级引用。
func cleanRealPath(realPath string) (string, error) {
	p := strings.TrimSpace(strings.ReplaceAll(realPath, `\`, "/"))
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", volume.ErrOutsideVolume
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return "", volume.ErrOutsideVolume // Windows 盘符绝对路径
	}
	c := path.Clean(p)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", volume.ErrOutsideVolume
	}
	return c, nil
}
