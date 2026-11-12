// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// inspect.go 实现 pkg/syncpkg.StorageInspector（真实存储审查）。
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
	"sync"

	"log/slog"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"golang.org/x/sync/errgroup"
)

// inspectMaxEntries 是单次反向解释的条目上限（防超大容器打爆响应/内存）。
const inspectMaxEntries = 50000

var _ syncpkg.StorageInspector = (*SecretdataFS)(nil)

// Trace 返回逻辑路径 rel 的真实落点（容器链 + meta + 分块 + parity + 去重引用）。
func (s *SecretdataFS) Trace(ctx context.Context, rel string, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
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
	// P5（2026-11-12）：P1 后 `metaEntry.meta` 为**常态 nil**（按需加载 + L1b 淘汰），
	// 而审查（traceFile/dedupRefCountLocked）需要真实落点与去重引用 ⇒ 先**锁外**确保已加载。
	// 失败不在此处报错：保持 `traceFile` 既有的 `AnomalyMetaUnreadable` 语义（异常上报）。
	s.mu.RLock()
	te := s.index[key]
	s.mu.RUnlock()
	if te != nil {
		_ = s.ensureEntryMeta(ctx, te)
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
func (s *SecretdataFS) traceUploading(up uploadProgress) *syncpkg.PathTrace {
	return &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: "uploading", Rel: up.rel, Kind: "file",
		Size:       up.total,
		Containers: s.containerChainByPath(up.container),
		Upload: &syncpkg.UploadProgress{
			Rel: up.rel, Container: up.container, Total: up.total,
			Committed: up.committed, Chunks: up.chunks, StartedAt: up.startedAt,
		},
	}
}

// containersForDir 返回逻辑目录 dirRel 的容器链（卷根 → 叶），零磁盘 IO（纯索引）。
func (s *SecretdataFS) containersForDir(dirRel string) []syncpkg.ContainerRef {
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
	out := make([]syncpkg.ContainerRef, 0, len(rels))
	for _, rel := range rels {
		seg, ok := s.dirSegs[rel]
		if !ok {
			continue
		}
		out = append(out, syncpkg.ContainerRef{RealPath: seg, LogicalDir: rel, DirID: s.dirIDs[rel]})
	}
	return out
}

// containerChainByPath 按**容器路径**逐层反查逻辑目录（索引未命中的层留空 LogicalDir）。
func (s *SecretdataFS) containerChainByPath(container string) []syncpkg.ContainerRef {
	if container == "" {
		return nil
	}
	segs := strings.Split(container, "/")
	out := make([]syncpkg.ContainerRef, 0, len(segs))
	prefix := ""
	for _, seg := range segs {
		if prefix == "" {
			prefix = seg
		} else {
			prefix = prefix + "/" + seg
		}
		ref := syncpkg.ContainerRef{RealPath: prefix}
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

// TraceTree 实现 syncpkg.TreeInspector：递归展开 depth 层子树（有界）。
//
// 语义：
//   - depth ≤ 1 等价于 Trace（零额外开销）；
//   - 目录的子项来自**逻辑索引**（ListDir），并额外并入**本进程在途上传**的直接子项
//     （索引里还没有，但用户应看到「正在上传」）；
//   - 超过 MaxTreeNodes 时截断，根节点附 anomaly:truncated（不静默丢）。
func (s *SecretdataFS) TraceTree(ctx context.Context, rel string, depth int, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	if depth > syncpkg.MaxTreeDepth {
		depth = syncpkg.MaxTreeDepth
	}
	root, err := s.Trace(ctx, rel, mode)
	if err != nil {
		return nil, err
	}
	if depth <= 1 {
		return root, nil
	}
	budget := syncpkg.MaxTreeNodes - 1
	s.expandTree(ctx, root, rel, depth-1, mode, &budget)
	return root, nil
}

// expandTree 递归填充 Children（预算用尽即停并标 truncated）。
func (s *SecretdataFS) expandTree(ctx context.Context, node *syncpkg.PathTrace, rel string, depth int, mode syncpkg.InspectMode, budget *int) {
	if depth <= 0 || *budget <= 0 || node.Kind != "dir" {
		return
	}
	for _, child := range s.treeChildren(ctx, rel, mode, budget) {
		node.Children = append(node.Children, *child)
		*budget--
		if *budget <= 0 {
			markTruncated(node, rel)
			return
		}
		s.expandTree(ctx, &node.Children[len(node.Children)-1], child.Rel, depth-1, mode, budget)
	}
}

// treeChildren 返回 rel 的直接子项（索引 + 在途上传），受预算约束。
func (s *SecretdataFS) treeChildren(ctx context.Context, rel string, _ syncpkg.InspectMode, budget *int) []*syncpkg.PathTrace {
	entries, err := s.ListDir(ctx, rel)
	if err != nil {
		// 目录暂时不可列举（并发删除等）：不中断整棵树，返回空。
		return nil
	}
	out := make([]*syncpkg.PathTrace, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, en := range entries {
		if *budget <= 0 {
			break
		}
		childRel := path.Join(rel, en.Name)
		if rel == "" {
			childRel = en.Name
		}
		seen[childRel] = struct{}{}
		child := s.traceChildIndexOnly(ctx, childRel)
		if child == nil {
			continue // 单条失败不拖垮整棵树
		}
		out = append(out, child)
	}
	// 在途上传（索引里还没有的直接子项）。
	for _, up := range s.uploadsUnder(rel) {
		if *budget <= 0 {
			break
		}
		if _, ok := seen[up.rel]; ok {
			continue
		}
		out = append(out, s.traceUploading(up))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}

// traceChildIndexOnly 是**纯索引**的子项视图（不做任何后端 IO）。
//
// 为什么不能复用 Trace：目录的 Trace 需要 ListDir 容器来定位 dir-meta 名（1 次后端调用）；
// 深度树对每个子目录都做一次 ⇒ 宽目录（数百子目录）在远端卷上退化为分钟级（实测 3m36s）。
// 子项只需要「名称/大小/类型/归属」，dir-meta 名属于细节 ⇒ 省略，保留 dir_id（来自索引）。
func (s *SecretdataFS) traceChildIndexOnly(ctx context.Context, rel string) *syncpkg.PathTrace {
	s.mu.RLock()
	e, isFile := s.index[rel]
	_, isDir := s.dirSegs[rel]
	var entry metaEntry
	if isFile && e != nil {
		entry = *e
	}
	mtime := s.dirMTime[rel]
	s.mu.RUnlock()
	switch {
	case isFile:
		return s.traceFile(ctx, rel, entry, 0, syncpkg.InspectIndex) // index 模式 = 纯索引
	case isDir:
		return &syncpkg.PathTrace{
			SchemaVersion: syncpkg.SchemaVersion, Mode: syncpkg.InspectIndex.String(),
			Rel: rel, Kind: "dir", MTime: mtime, Containers: s.containersForDir(rel),
		}
	default:
		return nil
	}
}

// uploadsUnder 返回逻辑父目录为 rel 的在途上传（直接子项）。
func (s *SecretdataFS) uploadsUnder(rel string) []uploadProgress {
	all := s.uploads.snapshotAll()
	out := make([]uploadProgress, 0, len(all))
	for _, up := range all {
		if parentDirOf(up.rel) == rel {
			out = append(out, up)
		}
	}
	return out
}

// markTruncated 标记子树截断（幂等：已有则不重复加）。
func markTruncated(node *syncpkg.PathTrace, rel string) {
	for _, a := range node.Anomalies {
		if a.Code == syncpkg.AnomalyTruncated {
			return
		}
	}
	node.Anomalies = append(node.Anomalies, syncpkg.Anomaly{
		Code: syncpkg.AnomalyTruncated, RealPath: rel,
		Detail: "子树节点数超过上限，已截断（用更小 depth 或指定子目录）",
	})
}

// traceFile 组装文件落点。
func (s *SecretdataFS) traceFile(ctx context.Context, rel string, e metaEntry, dedupRefs int, mode syncpkg.InspectMode) *syncpkg.PathTrace {
	tr := &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: mode.String(), Rel: rel, Kind: "file",
		Size: e.size, MTime: e.mtime,
		Containers: s.containersForDir(parentDirOf(rel)),
	}
	if e.meta == nil {
		tr.Anomalies = append(tr.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyMetaUnreadable, RealPath: path.Join(e.dirSeg, e.metaName),
			Detail: "索引条目缺少 meta（可能未加载完成）",
		})
		return tr
	}
	if e.dataDir != "" {
		tr.Dedup = &syncpkg.DedupRef{Container: e.dataDir, Refs: dedupRefs}
		tr.Anomalies = append(tr.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyDedupShared, RealPath: e.dataDir,
			Detail: "内容分块位于卷级去重容器（共享 blob）",
		})
	}
	meta := syncpkg.BlobRef{RealPath: path.Join(e.dirSeg, e.metaName), Bytes: metaBlobBytes(&e)}
	tr.Meta = &meta

	dataSeg := e.dataDir
	if dataSeg == "" {
		dataSeg = e.dirSeg
	}
	for i := range e.meta.Chunks {
		c := e.meta.Chunks[i]
		tr.Chunks = append(tr.Chunks, syncpkg.ChunkRef{
			Index:     c.Index,
			Blob:      syncpkg.BlobRef{RealPath: path.Join(dataSeg, c.FileName), Bytes: c.EncSize, Plain: c.OrigSize},
			Blocklets: len(c.Blocklets),
		})
	}
	if p := e.meta.Parity; p != nil {
		tr.Parity = &syncpkg.BlobRef{RealPath: path.Join(dataSeg, p.FileName), Bytes: p.EncSize, Plain: p.PlainLen}
	}
	s.probeTraceBlobs(ctx, tr, mode)
	sortAnomalies(tr.Anomalies)
	return tr
}

// inspectProbeConcurrency 是 disk 模式探测的并发上限。
//
// 为什么需要并发：远端卷每个 blob 一次 Stat 往返（网盘后端甚至起一次 CLI 进程）；大文件
// 上千个分块时串行探测是分钟级。并发 8 让 RTT 重叠（与仓库既有上传并发策略同量级）。
const inspectProbeConcurrency = 8

// probeTraceBlobs 在 disk 模式下**有界并发**探测全部 blob（保持结果顺序：只回填各自 ref）。
func (s *SecretdataFS) probeTraceBlobs(ctx context.Context, tr *syncpkg.PathTrace, mode syncpkg.InspectMode) {
	if mode != syncpkg.InspectDisk {
		return
	}
	refs := make([]*syncpkg.BlobRef, 0, len(tr.Chunks)+2)
	if tr.Meta != nil {
		refs = append(refs, tr.Meta)
	}
	for i := range tr.Chunks {
		refs = append(refs, &tr.Chunks[i].Blob)
	}
	if tr.Parity != nil {
		refs = append(refs, tr.Parity)
	}
	if len(refs) > inspectMaxEntries {
		// 超大文件：截断探测（其余 blob 标为未探测），避免一次请求打爆后端/内存。
		tr.Anomalies = append(tr.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyTruncated, RealPath: tr.Rel,
			Detail: fmt.Sprintf("分块数 %d 超过 %d，disk 探测已截断（其余标为未知）", len(refs), inspectMaxEntries),
		})
		refs = refs[:inspectMaxEntries]
	}
	probeRefs(ctx, s, refs, tr, mode)
}

// probeRefs 有界并发探测 refs（每 goroutine 只写自己的 ref，异常经互斥收集）。
func probeRefs(ctx context.Context, s *SecretdataFS, refs []*syncpkg.BlobRef, tr *syncpkg.PathTrace, mode syncpkg.InspectMode) {
	if len(refs) == 0 {
		return
	}
	var mu sync.Mutex
	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(inspectProbeConcurrency)
	for _, ref := range refs {
		eg.Go(func() error {
			if a := probeBlob(gctx, s, ref, mode); a != nil {
				mu.Lock()
				tr.Anomalies = append(tr.Anomalies, *a)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = eg.Wait()
}

// sortAnomalies 稳定排序（Code, RealPath），保证并发探测下输出可复现。
func sortAnomalies(list []syncpkg.Anomaly) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Code != list[j].Code {
			return list[i].Code < list[j].Code
		}
		return list[i].RealPath < list[j].RealPath
	})
}

// traceDir 组装目录落点（容器 + dir-meta blob）。
func (s *SecretdataFS) traceDir(ctx context.Context, rel, container string, mtime int64, _ int, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	tr := &syncpkg.PathTrace{
		SchemaVersion: syncpkg.SchemaVersion, Mode: mode.String(), Rel: rel, Kind: "dir", MTime: mtime,
		Containers: s.containersForDir(rel),
	}
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 读取容器 %q 失败: %w", container, err)
	}
	dirMetas := collectDirMetas(entries)
	recordDirMetasState(tr, container, dirMetas)
	if len(dirMetas) > 0 {
		tr.Meta = s.probeDirMeta(ctx, container, dirMetas[0], entries, tr, mode)
	}
	return tr, nil
}

// collectDirMetas 收集容器内全部 dir-meta 名（正常恰好 1 个）。
func collectDirMetas(entries []syncpkg.Entry) []string {
	var out []string
	for _, en := range entries {
		if shardseal.ClassifyName(en.Name) == shardseal.KindDirMeta {
			out = append(out, en.Name)
		}
	}
	return out
}

// recordDirMetasState 记录「无 dir-meta / 多个 dir-meta」异常。
func recordDirMetasState(tr *syncpkg.PathTrace, container string, dirMetas []string) {
	switch {
	case len(dirMetas) == 0:
		tr.Anomalies = append(tr.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyContainerNotIndexed, RealPath: container,
			Detail: "容器内未找到 dir-meta blob（无法确定逻辑目录）",
		})
	case len(dirMetas) > 1:
		tr.Anomalies = append(tr.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyDirMetaAmbiguous, RealPath: container,
			Detail: fmt.Sprintf("容器内有 %d 个 dir-meta blob（异常布局）", len(dirMetas)),
		})
	}
}

// probeDirMeta 构造 dir-meta 的 BlobRef（disk 模式探测一次），返回该引用。
func (s *SecretdataFS) probeDirMeta(ctx context.Context, container, name string, entries []syncpkg.Entry, tr *syncpkg.PathTrace, mode syncpkg.InspectMode) *syncpkg.BlobRef {
	size := int64(0)
	for _, en := range entries {
		if en.Name == name {
			size = en.Size
			break
		}
	}
	blob := syncpkg.BlobRef{RealPath: path.Join(container, name), Bytes: size}
	if a := probeBlob(ctx, s, &blob, mode); a != nil {
		tr.Anomalies = append(tr.Anomalies, *a)
	}
	return &blob
}

// Explain 解释真实路径的用途与逻辑归属。
func (s *SecretdataFS) Explain(ctx context.Context, realArg string, mode syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
	realPath, err := cleanRealPath(realArg)
	if err != nil {
		return nil, err
	}
	ex := &syncpkg.RealExplanation{
		SchemaVersion: syncpkg.SchemaVersion, Mode: mode.String(), RealPath: realPath,
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
			ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
				Code: syncpkg.AnomalyNotFound, RealPath: realPath,
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
func (s *SecretdataFS) explainContainer(ctx context.Context, ex *syncpkg.RealExplanation, realPath string, mode syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
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
	idx := s.buildContainerIndex(ctx, realPath)
	seen := make(map[string]struct{}, len(entries))
	out := make([]syncpkg.RealEntry, len(entries))
	// 子容器解析要读其 dir-meta（每子容器 1 次 ListDir + 1 次读），串行会在宽目录下退化为
	// N 次往返 ⇒ 有界并发；非目录条目就地判定（顺序写各自下标，异常仍写 ex，先于并发段完成）。
	dirs := make([]int, 0, len(entries))
	for i, en := range entries {
		seen[en.Name] = struct{}{}
		if en.IsDir {
			dirs = append(dirs, i)
			continue
		}
		out[i] = s.classifyRealEntry(ctx, realPath, en, dirMetaName, ex, idx)
	}
	s.resolveSubContainers(ctx, realPath, entries, dirs, out)
	ex.Entries = append(ex.Entries, out...)
	if mode == syncpkg.InspectDisk {
		appendMissingBlobs(ex, realPath, idx.expected, seen)
	}
	return ex, nil
}

// sortAndTruncateEntries 排序并截断超限条目（附 truncated 异常）。
func sortAndTruncateEntries(entries []syncpkg.Entry, ex *syncpkg.RealExplanation, realPath string) []syncpkg.Entry {
	if len(entries) > inspectMaxEntries {
		entries = entries[:inspectMaxEntries]
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyTruncated, RealPath: realPath,
			Detail: fmt.Sprintf("条目超过 %d，已截断", inspectMaxEntries),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// resolveSubContainers 有界并发解析子容器条目（结果按下标回填，顺序稳定）。
func (s *SecretdataFS) resolveSubContainers(ctx context.Context, container string, entries []syncpkg.Entry, dirs []int, out []syncpkg.RealEntry) {
	if len(dirs) == 0 {
		return
	}
	eg, gctx := errgroup.WithContext(ctx)
	eg.SetLimit(inspectProbeConcurrency)
	for _, i := range dirs {
		eg.Go(func() error {
			out[i] = s.classifySubContainer(gctx, container, entries[i])
			return nil
		})
	}
	_ = eg.Wait()
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
func recordContainerDirMetaState(ex *syncpkg.RealExplanation, realPath, dirMetaName string, dirMetaCount int) {
	switch {
	case dirMetaCount == 0:
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyContainerNotIndexed, RealPath: realPath,
			Detail: "容器内无 dir-meta（无法确定逻辑目录）",
		})
	case dirMetaCount > 1:
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyDirMetaAmbiguous, RealPath: realPath,
			Detail: fmt.Sprintf("容器内有 %d 个 dir-meta blob（异常布局）", dirMetaCount),
		})
	}
	_ = dirMetaName
}

// classifyRealEntry 判定容器内单个条目的用途与归属。
func (s *SecretdataFS) classifyRealEntry(ctx context.Context, container string, en syncpkg.Entry, dirMetaName string, ex *syncpkg.RealExplanation, idx containerIndex) syncpkg.RealEntry {
	// 目录条目 = **子容器**（逻辑目录），不是 blob：必须单独判定，否则会被误报为孤儿分块。
	if en.IsDir {
		return s.classifySubContainer(ctx, container, en)
	}
	switch shardseal.ClassifyName(en.Name) {
	case shardseal.KindDirMeta:
		return dirMetaEntry(en, dirMetaName, ex)
	case shardseal.KindFileMeta:
		return s.classifyFileMetaEntry(container, en, ex, idx)
	default:
		return s.classifyChunkEntry(container, en, ex, idx)
	}
}

// dirMetaEntry 判定 dir-meta 条目（本目录自身的加密 meta）。
func dirMetaEntry(en syncpkg.Entry, dirMetaName string, ex *syncpkg.RealExplanation) syncpkg.RealEntry {
	re := syncpkg.RealEntry{Name: en.Name, Kind: "dir_meta", Bytes: en.Size, Note: "本目录的加密 meta"}
	if ex.Container != nil && en.Name == dirMetaName {
		re.Note = "本目录的加密 meta（dir_id=" + ex.Container.DirID + "）"
	}
	return re
}

func (s *SecretdataFS) classifyFileMetaEntry(container string, en syncpkg.Entry, ex *syncpkg.RealExplanation, idx containerIndex) syncpkg.RealEntry {
	re := syncpkg.RealEntry{Name: en.Name, Kind: "file_meta", Bytes: en.Size}
	if owner := idx.fileOwners[en.Name]; owner != nil {
		re.Owner = owner
		re.Note = "文件 meta（逻辑路径见 owner）"
		return re
	}
	re.Note = "文件 meta 但索引无对应条目（索引未加载/外部写入）"
	ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
		Code: syncpkg.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
		Detail: "file-meta 无法反查到逻辑文件；疑似孤儿（删除请走 GC）",
	})
	return re
}

// classifyChunkEntry 判定内容分块/parity：索引命中 → 正常归属；
// 否则若属于**本进程在途上传** → upload_in_progress（不是孤儿）；再否则 → blob_orphan。
func (s *SecretdataFS) classifyChunkEntry(container string, en syncpkg.Entry, ex *syncpkg.RealExplanation, idx containerIndex) syncpkg.RealEntry {
	re := syncpkg.RealEntry{Name: en.Name, Kind: "chunk", Bytes: en.Size}
	owner := idx.chunkOwners[en.Name]
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
		re.Owner = &syncpkg.LogicalRef{Rel: up.rel, Role: "file"}
		re.Note = "上传进行中（meta 尚未提交；提交后成为正式分块）"
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyUploadInProgress, RealPath: path.Join(container, en.Name),
			Detail: "属于本进程在途上传（rel=" + up.rel + "）；不是孤儿，请勿清理",
		})
		return re
	}
	re.Note = "内容分块/parity 但索引无引用；疑似孤儿（删除请走 GC）"
	ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
		Code: syncpkg.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
		Detail: "分块无索引引用；疑似孤儿（删除请走 GC）",
	})
	return re
}

// classifySubContainer 判定子容器目录条目。
func (s *SecretdataFS) classifySubContainer(ctx context.Context, container string, en syncpkg.Entry) syncpkg.RealEntry {
	re := syncpkg.RealEntry{Name: en.Name, Kind: "container", Bytes: en.Size}
	child := path.Join(container, en.Name)
	if ref, ok := s.resolveChildContainer(ctx, child); ok {
		re.Container = ref
		re.Note = "子容器（逻辑目录 " + displayDirName(ref.LogicalDir) + "）"
	} else {
		re.Note = "子容器（dir-meta 未解出）"
	}
	return re
}

// uploadInContainer 报告该容器是否有本进程在途上传。
func (s *SecretdataFS) uploadInContainer(container string) bool {
	return len(s.uploadsForContainer(container)) > 0
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
func (s *SecretdataFS) resolveChildContainer(ctx context.Context, container string) (*syncpkg.ContainerRef, bool) {
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
		ref := &syncpkg.ContainerRef{RealPath: container, DirID: dm.DirID, ParentID: dm.ParentDirID}
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
func appendMissingBlobs(ex *syncpkg.RealExplanation, container string, expected, seen map[string]struct{}) {
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyBlobMissing, RealPath: path.Join(container, name),
			Detail: "索引声明该 blob 应存在，但磁盘上缺失",
		})
	}
}

// explainBlob 解释单个真实文件（dir-meta / file-meta / 分块 / parity）。
func (s *SecretdataFS) explainBlob(ctx context.Context, ex *syncpkg.RealExplanation, realPath string, _ syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
	base := path.Base(realPath)
	container := path.Dir(realPath)
	if container == "." {
		container = ""
	}
	switch shardseal.ClassifyName(base) {
	case shardseal.KindDirMeta, shardseal.KindFileMeta:
		// 只有 meta 需要内容（解密）；**分块绝不读内容**——单块可达 100+ MiB，
		// 远端卷上一次 explain 就会拉整块（旧实现缺陷）。分块判型/归属纯靠名字 + 索引。
		blob, err := readBlob(ctx, s.inner, realPath)
		if err != nil {
			return nil, fmt.Errorf("secretdata: 读取 blob %q 失败: %w", realPath, err)
		}
		if shardseal.ClassifyName(base) == shardseal.KindDirMeta {
			s.explainDirMetaBlob(ctx, ex, container, base, blob)
		} else {
			s.explainFileMetaBlob(ex, container, base, realPath, blob)
		}
	default:
		s.explainChunkBlob(ex, container, base, realPath)
	}
	return ex, nil
}

// explainDirMetaBlob 解释 dir-meta blob（容器逻辑归属）。
func (s *SecretdataFS) explainDirMetaBlob(ctx context.Context, ex *syncpkg.RealExplanation, container, base string, blob []byte) {
	ex.Kind = "dir_meta"
	dm, derr := s.decryptDirMeta(blob)
	if derr != nil {
		_ = dm
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyMetaUnreadable, RealPath: path.Join(container, base), Detail: derr.Error(),
		})
		return
	}
	ex.Container, ex.Evidence = s.resolveContainer(ctx, container, base, ex)
}

// explainFileMetaBlob 解释 file-meta blob（解 meta + 索引反查归属）。
func (s *SecretdataFS) explainFileMetaBlob(ex *syncpkg.RealExplanation, container, base, realPath string, blob []byte) {
	ex.Kind = "file_meta"
	mm, derr := s.decryptFileMeta(blob)
	if derr != nil {
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyMetaUnreadable, RealPath: realPath, Detail: derr.Error(),
		})
		return
	}
	ex.Evidence = append(ex.Evidence,
		fmt.Sprintf("meta Original.Name=%s size=%d chunks=%d", mm.Original.Name, mm.Original.Size, len(mm.Chunks)))
	switch owner := s.lookupFileByMetaName(container, base); {
	case owner != nil:
		ex.Owner = owner
		ex.Evidence = append(ex.Evidence, "索引反查命中（容器 + meta 名）")
	case s.uploadInContainer(container):
		up, _ := s.uploadForContainer(container)
		ex.Kind = "upload_in_progress"
		ex.Owner = &syncpkg.LogicalRef{Rel: up.rel, Role: "file"}
		ex.Evidence = append(ex.Evidence, "命中本进程在途上传注册表（meta 已上传、索引未提交）")
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyUploadInProgress, RealPath: realPath,
			Detail: "属于本进程在途上传（rel=" + up.rel + "）；不是孤儿，请勿清理",
		})
	default:
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyBlobOrphan, RealPath: realPath,
			Detail: "meta 可解密但索引无对应逻辑文件（索引未加载/外部写入）；疑似孤儿（删除请走 GC）",
		})
	}
	if c, ok := s.containerOfDir(container); ok {
		ex.Container = c
	}
}

// explainChunkBlob 解释内容分块/parity（命中索引则给出归属，未命中记孤儿）。
func (s *SecretdataFS) explainChunkBlob(ex *syncpkg.RealExplanation, container, base, realPath string) {
	ex.Kind = "chunk"
	owner := s.lookupChunkOwner(container, base)
	switch {
	case owner == nil:
		if up, ok := s.uploadForContainer(container); ok {
			ex.Kind = "upload_in_progress"
			ex.Owner = &syncpkg.LogicalRef{Rel: up.rel, Role: "file"}
			ex.Evidence = append(ex.Evidence, "命中本进程在途上传注册表（meta 尚未提交）")
			ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
				Code: syncpkg.AnomalyUploadInProgress, RealPath: realPath,
				Detail: "属于本进程在途上传（rel=" + up.rel + "）；不是孤儿，请勿清理",
			})
			if c, ok := s.containerOfDir(container); ok {
				ex.Container = c
			}
			return
		}
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyBlobOrphan, RealPath: realPath,
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
func (s *SecretdataFS) resolveContainer(ctx context.Context, container, dirMetaName string, ex *syncpkg.RealExplanation) (*syncpkg.ContainerRef, []string) {
	ref := &syncpkg.ContainerRef{RealPath: container}
	blob, err := readBlob(ctx, s.inner, path.Join(container, dirMetaName))
	if err != nil {
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyMetaUnreadable, RealPath: path.Join(container, dirMetaName), Detail: err.Error(),
		})
		return ref, nil
	}
	dm, derr := s.decryptDirMeta(blob)
	if derr != nil {
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyMetaUnreadable, RealPath: path.Join(container, dirMetaName), Detail: derr.Error(),
		})
		return ref, nil
	}
	ref.DirID = dm.DirID
	ref.ParentID = dm.ParentDirID
	rel, ok := s.resolveDirByID(dm.DirID)
	if !ok {
		ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
			Code: syncpkg.AnomalyContainerNotIndexed, RealPath: container,
			Detail: "dir-meta 的 dir_id 在索引中不存在（索引未加载或外部创建）",
		})
		return ref, []string{"dir-meta 解密成功 dir_id=" + dm.DirID}
	}
	ref.LogicalDir = rel
	evidence := []string{"dir-meta 解密成功 dir_id=" + dm.DirID + " name=" + dm.Name}
	if dm.ParentDirID != "" {
		if _, parentOK := s.resolveDirByID(dm.ParentDirID); !parentOK {
			ex.Anomalies = append(ex.Anomalies, syncpkg.Anomaly{
				Code: syncpkg.AnomalyParentUnresolved, RealPath: container,
				Detail: "dir-meta 的 parent_dir_id 在索引中缺失: " + dm.ParentDirID,
			})
		}
	}
	return ref, evidence
}

// containerIndex 是**一次遍历**构建的容器视图：归属映射 + 期望 blob 集合。
//
// 为什么需要：旧实现对容器内**每条** blob 各做一次全索引扫描（lookupChunkOwner /
// containerIndex 是**一次遍历**构建的容器视图：归属映射 + 期望 blob 集合。
//
// 为什么需要：旧实现对容器内**每条** blob 各做一次全索引扫描（lookupChunkOwner /
// lookupFileByMetaName）⇒ O(容器条目数 × 全卷条目数)，大容器/大卷上是分钟级且持锁
// 反复加解锁。现在整次 Explain 只扫一遍索引。
type containerIndex struct {
	fileOwners  map[string]*syncpkg.LogicalRef // file-meta 名 → 逻辑文件
	chunkOwners map[string]*syncpkg.LogicalRef // 分块/parity 名 → 逻辑文件
	expected    map[string]struct{}            // 索引声明应出现在该容器的 blob 名（disk 差异）
}

// buildContainerIndex 遍历一次索引，收集与 container 相关的全部映射。
// buildContainerIndex 构建容器视图（哪些索引条目属于本容器）。
//
// P5（2026-11-12）：`addEntry`/`chunkOwnerOfEntry` 需要 `Chunks`/`Parity`，而 P1 后
// `metaEntry.meta` 常态为 nil ⇒ 先在**锁外**确保**本容器内**条目的 meta 已加载
// （只取 `dirSeg == container` 的条目，避免不必要的全量加载）。
func (s *SecretdataFS) buildContainerIndex(ctx context.Context, container string) containerIndex {
	s.mu.RLock()
	cands := make([]*metaEntry, 0, 8)
	for _, e := range s.index {
		if e.dirSeg == container {
			cands = append(cands, e)
		}
	}
	s.mu.RUnlock()
	for _, e := range cands {
		_ = s.ensureEntryMeta(ctx, e)
	}
	idx := containerIndex{
		fileOwners:  make(map[string]*syncpkg.LogicalRef),
		chunkOwners: make(map[string]*syncpkg.LogicalRef),
		expected:    make(map[string]struct{}),
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, e := range s.index {
		idx.addEntry(rel, e, container)
	}
	return idx
}

// addEntry 把单个索引条目并入容器视图（仅当它属于 container）。
func (idx *containerIndex) addEntry(rel string, e *metaEntry, container string) {
	if e == nil || e.meta == nil {
		return
	}
	if e.dirSeg == container {
		idx.fileOwners[e.metaName] = &syncpkg.LogicalRef{Rel: rel, Role: "file"}
		idx.expected[e.metaName] = struct{}{}
	}
	if e.dataSegPath() != container {
		return
	}
	idx.addParity(rel, e.meta.Parity)
	for i := range e.meta.Chunks {
		idx.addChunk(rel, i, e.meta.Chunks[i])
	}
}

// addParity 记录 parity 分块归属。
func (idx *containerIndex) addParity(rel string, p *shardseal.ParityInfo) {
	if p == nil {
		return
	}
	idx.chunkOwners[p.FileName] = &syncpkg.LogicalRef{Rel: rel, Role: "file", Parity: true}
	idx.expected[p.FileName] = struct{}{}
}

// addChunk 记录内容分块归属（Index<0 时用位置兜底，与读路径同口径）。
func (idx *containerIndex) addChunk(rel string, pos int, c shardseal.ChunkInfo) {
	ci := c.Index
	if ci < 0 {
		ci = pos
	}
	idx.chunkOwners[c.FileName] = &syncpkg.LogicalRef{Rel: rel, Role: "file", ChunkIndex: &ci}
	idx.expected[c.FileName] = struct{}{}
}

// dataSegPath 返回条目内容所在容器（去重条目指向卷级池容器）。
func (e *metaEntry) dataSegPath() string {
	if e.dataDir != "" {
		return e.dataDir
	}
	return e.dirSeg
}

// lookupFileByMetaName 反查「容器 + meta 名 → 逻辑文件」（单次请求用；整容器解释走 containerIndex）。
func (s *SecretdataFS) lookupFileByMetaName(container, metaName string) *syncpkg.LogicalRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, e := range s.index {
		if e == nil || e.dirSeg != container || e.metaName != metaName {
			continue
		}
		return &syncpkg.LogicalRef{Rel: rel, Role: "file"}
	}
	return nil
}

// lookupChunkOwner 反查「容器 + 分块名 → 逻辑文件」（单次请求用；整容器解释走 containerIndex）。
//
// parity 与普通分块**命名同形**：只能靠 meta.Parity.FileName 判定（不能按名字猜）。
func (s *SecretdataFS) lookupChunkOwner(container, name string) *syncpkg.LogicalRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for rel, e := range s.index {
		if owner := chunkOwnerOfEntry(rel, e, container, name); owner != nil {
			return owner
		}
	}
	return nil
}

// chunkOwnerOfEntry 判定单条目是否拥有该分块名。
func chunkOwnerOfEntry(rel string, e *metaEntry, container, name string) *syncpkg.LogicalRef {
	if e == nil || e.meta == nil || e.dataSegPath() != container {
		return nil
	}
	if p := e.meta.Parity; p != nil && p.FileName == name {
		return &syncpkg.LogicalRef{Rel: rel, Role: "file", Parity: true}
	}
	for i := range e.meta.Chunks {
		if e.meta.Chunks[i].FileName != name {
			continue
		}
		ci := e.meta.Chunks[i].Index
		if ci < 0 {
			ci = i
		}
		return &syncpkg.LogicalRef{Rel: rel, Role: "file", ChunkIndex: &ci}
	}
	return nil
}

// containerOfDir 反查「容器 → 逻辑目录」。
func (s *SecretdataFS) containerOfDir(container string) (*syncpkg.ContainerRef, bool) {
	rel, ok := s.dirOfContainer(container)
	if !ok {
		return nil, false
	}
	ref := &syncpkg.ContainerRef{RealPath: container, LogicalDir: rel}
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

// probeBlob 在 disk 模式下探测单个 blob 的存在性与大小，返回差异（nil = 正常/非 disk）。
//
// 返回而非直接 append：调用方可能并发探测（各自 ref 独立写，异常由调用方汇总）。
func probeBlob(ctx context.Context, s *SecretdataFS, ref *syncpkg.BlobRef, mode syncpkg.InspectMode) *syncpkg.Anomaly {
	if mode != syncpkg.InspectDisk {
		return nil
	}
	ent, err := s.inner.Stat(ctx, ref.RealPath)
	if err == nil && ent == nil {
		err = fs.ErrNotExist // FS 契约：不存在可能返回 (nil, nil)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			present := false
			ref.OnDisk = &present
			return &syncpkg.Anomaly{
				Code: syncpkg.AnomalyBlobMissing, RealPath: ref.RealPath,
				Detail: "索引/meta 声明该 blob，但磁盘上缺失",
			}
		}
		return &syncpkg.Anomaly{
			Code: syncpkg.AnomalyProbeFailed, RealPath: ref.RealPath,
			Detail: "探测失败（存在性/大小未知）: " + err.Error(),
		}
	}
	present := true
	ref.OnDisk = &present
	size := ent.Size
	ref.DiskBytes = &size
	if ref.Bytes > 0 && size != ref.Bytes {
		return &syncpkg.Anomaly{
			Code: syncpkg.AnomalySizeMismatch, RealPath: ref.RealPath,
			Detail: fmt.Sprintf("磁盘 %d 字节 ≠ 声明 %d 字节", size, ref.Bytes),
		}
	}
	return nil
}

// cleanLogicalRel 规范化逻辑路径；含父级引用 ⇒ ErrOutsideVolume。
func cleanLogicalRel(rel string) (string, error) {
	p := strings.Trim(strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/")), "/")
	if p == "" {
		return "", nil
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", syncpkg.ErrOutsideVolume
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
		return "", syncpkg.ErrOutsideVolume
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return "", syncpkg.ErrOutsideVolume // Windows 盘符绝对路径
	}
	c := path.Clean(p)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", syncpkg.ErrOutsideVolume
	}
	return c, nil
}
