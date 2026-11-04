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
	"sort"
	"strings"

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
	s.mu.RLock()
	e, isFile := s.index[key]
	var entry metaEntry
	if isFile && e != nil {
		entry = *e
	}
	container, isDir := s.dirSegs[key]
	isDir = isDir || key == ""
	dirMTime := s.dirMTime[key]
	dedupRefs := 0
	if isFile && entry.dataDir != "" && entry.meta != nil && len(entry.meta.Chunks) > 0 {
		want := entry.meta.Chunks[0].FileName
		for _, other := range s.index {
			if other != nil && other.dataDir == entry.dataDir && other.meta != nil &&
				len(other.meta.Chunks) > 0 && other.meta.Chunks[0].FileName == want {
				dedupRefs++
			}
		}
	}
	s.mu.RUnlock()

	switch {
	case isFile:
		return s.traceFile(ctx, key, entry, dedupRefs, mode), nil
	case isDir:
		return s.traceDir(ctx, key, container, dirMTime, dedupRefs, mode)
	default:
		return nil, fmt.Errorf("secretdata: 逻辑路径不存在: %w", fs.ErrNotExist)
	}
}

// traceFile 组装文件落点。
func (s *SecretdataFS) traceFile(ctx context.Context, rel string, e metaEntry, dedupRefs int, mode volume.InspectMode) *volume.PathTrace {
	tr := &volume.PathTrace{
		SchemaVersion: volume.SchemaVersion, Mode: mode.String(), Rel: rel, Kind: "file",
		Size: e.size, MTime: e.mtime,
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
		ex.Entries = append(ex.Entries, s.classifyRealEntry(realPath, en, dirMetaName, ex))
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
func (s *SecretdataFS) classifyRealEntry(container string, en syncpkg.Entry, dirMetaName string, ex *volume.RealExplanation) volume.RealEntry {
	re := volume.RealEntry{Name: en.Name, Kind: "unknown", Bytes: en.Size}
	switch shardseal.ClassifyName(en.Name) {
	case shardseal.KindDirMeta:
		re.Kind = "dir_meta"
		re.Note = "本目录的加密 meta"
		if ex.Container != nil && en.Name == dirMetaName {
			re.Note = "本目录的加密 meta（dir_id=" + ex.Container.DirID + "）"
		}
	case shardseal.KindFileMeta:
		re.Kind = "file_meta"
		if owner := s.lookupFileByMetaName(container, en.Name); owner != nil {
			re.Owner = owner
			re.Note = "文件 meta（逻辑路径见 owner）"
		} else {
			re.Note = "文件 meta 但索引无对应条目（索引未加载/外部写入）"
			ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
				Code: volume.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
				Detail: "file-meta 无法反查到逻辑文件；疑似孤儿（删除请走 GC）",
			})
		}
	default:
		re.Kind = "chunk"
		owner := s.lookupChunkOwner(container, en.Name)
		switch {
		case owner == nil:
			re.Note = "内容分块/parity 但索引无引用；疑似孤儿（删除请走 GC）"
			ex.Anomalies = append(ex.Anomalies, volume.Anomaly{
				Code: volume.AnomalyBlobOrphan, RealPath: path.Join(container, en.Name),
				Detail: "分块无索引引用；疑似孤儿（删除请走 GC）",
			})
		case owner.Parity:
			re.Kind = "parity"
			re.Note = "XOR 奇偶校验段"
			re.Owner = owner
		default:
			re.Note = "内容分块"
			re.Owner = owner
		}
	}
	return re
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
