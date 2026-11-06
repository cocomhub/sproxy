// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// dup_export.go —— 重复容器争议内容的**解码导出**（2026-11-07 用户要求）。
//
// 背景：重复容器组的「落选容器」数据保留但**未加载**（不在索引里）。要判断它们是否含
// 权威容器没有的**真实内容**（而不只是物理条目名差异），必须把它们**解密导出为真实文件**
// 供人工核对后再决定处置（删除/合并/保留）。
//
// **同名文件隔离**（用户明确提醒）：不同容器的文件名可能完全相同 ⇒ 每个容器导出到
// **独立子目录**，路径中编码「逻辑目录 + 容器相对路径」，保证：
//
//	<dstRoot>/<逻辑目录安全化>/<落选容器标识>/<容器内相对路径安全化>/<原始文件名>
//
// 同一容器内不同子容器也各有子目录 ⇒ 全局无覆盖。

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// dupExportTempPrefix 是导出期间临时注册的伪逻辑路径前缀（与真实 rel 隔离，导出后清理）。
const dupExportTempPrefix = "__dup_export__/"

// ExportDuplicateContainers 把所有**重复容器组**的落选容器递归解码导出到 dstRoot。
//
// 返回 (导出文件数, 导出字节数, error)。仅在确有争议（或全体）时由运维显式触发。
// 结构见文件头注释（每个容器独立子目录 ⇒ 同名文件天然隔离、不覆盖）。
func (s *SecretdataFS) ExportDuplicateContainers(ctx context.Context, dstRoot string) (int, int64, error) {
	if dstRoot == "" {
		return 0, 0, fmt.Errorf("secretdata: 导出目录不能为空")
	}
	groups := s.dupGroupsSnapshot()
	if len(groups) == 0 {
		return 0, 0, nil
	}
	totalFiles, totalBytes := 0, int64(0)
	for _, g := range groups {
		for _, o := range g.others {
			n, b, err := s.ExportDuplicateContainer(ctx, g.rel, o, dstRoot)
			if err != nil {
				return totalFiles, totalBytes, err
			}
			totalFiles += n
			totalBytes += b
			slog.Info("secretdata: 重复容器导出完成", "rel", g.rel, "container", o, "files", n, "bytes", b)
		}
	}
	return totalFiles, totalBytes, nil
}

// ExportDuplicateContainer 导出**单个落选容器**（含其全部子容器）的解码文件到 dstRoot。
func (s *SecretdataFS) ExportDuplicateContainer(ctx context.Context, groupRel, container, dstRoot string) (int, int64, error) {
	nodes, err := s.collectContainerTree(ctx, container)
	if err != nil {
		return 0, 0, err
	}
	files, bytes := 0, int64(0)
	for _, n := range nodes {
		f, b, derr := s.exportOneContainer(ctx, groupRel, container, n.container, n.suffix, dstRoot)
		if derr != nil {
			return files, bytes, derr
		}
		files += f
		bytes += b
	}
	return files, bytes, nil
}

// dupExportNode 是待导出的容器节点（container = 物理路径；suffix = 相对根容器的子路径）。
type dupExportNode struct {
	container string
	suffix    string
}

// collectContainerTree 递归收集容器及其全部子容器（物理扫描；前序）。
func (s *SecretdataFS) collectContainerTree(ctx context.Context, root string) ([]dupExportNode, error) {
	queue := []dupExportNode{{container: root}}
	var out []dupExportNode
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		out = append(out, n)
		subs, err := s.listInner(ctx, n.container)
		if err != nil {
			return nil, fmt.Errorf("secretdata: 列举容器 %q 失败: %w", n.container, err)
		}
		for _, e := range subs {
			if !e.IsDir {
				continue
			}
			queue = append(queue, dupExportNode{
				container: path.Join(n.container, e.Name),
				suffix:    path.Join(n.suffix, e.Name),
			})
		}
	}
	return out, nil
}

// exportOneContainer 导出单个容器内的全部文件（临时注册 + 解密 + 写本地 + 清理）。
// exportOneContainer 导出单个容器内的全部文件。
// **同名隔离**：路径含「逻辑目录 + **落选容器标识** + 容器内相对路径 + 文件名」——
// 不同落选容器常有同名文件（如都叫 `X.mp4`），必须靠容器标识区分（用户明确要求）。
func (s *SecretdataFS) exportOneContainer(ctx context.Context, groupRel, containerRoot, container, suffix, dstRoot string) (int, int64, error) {
	tmpRel := dupExportTempPrefix + sanitizeExportSegment(container)
	// 临时注册（供 OpenRead 拼 blob 物理路径）；导出后清理（见 defer）
	s.mu.Lock()
	prevSeg, hadSeg := s.dirSegs[tmpRel]
	s.dirSegs[tmpRel] = container
	s.dirs[tmpRel] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for k := range s.index {
			if strings.HasPrefix(k, tmpRel+"/") {
				delete(s.index, k)
			}
		}
		if hadSeg {
			s.dirSegs[tmpRel] = prevSeg
		} else {
			delete(s.dirSegs, tmpRel)
		}
		delete(s.dirs, tmpRel)
		s.mu.Unlock()
	}()
	if err := s.loadContainerFileMetas(ctx, container, tmpRel); err != nil {
		return 0, 0, err
	}
	keys := s.indexKeysUnder(tmpRel)

	// **同名隔离**：<dstRoot>/<逻辑目录>/<落选容器标识>/<容器内相对路径>/<文件名>
	outDir := filepath.Join(dstRoot,
		sanitizeExportSegment(groupRel),
		sanitizeExportSegment(path.Base(containerRoot)),
		sanitizeExportSegment(suffix))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, 0, err
	}
	files, bytes := 0, int64(0)
	for _, k := range keys {
		name := strings.TrimPrefix(k, tmpRel+"/")
		_, b, werr := s.exportOneFile(ctx, k, filepath.Join(outDir, sanitizeExportSegment(name)))
		if werr != nil {
			return files, bytes, werr
		}
		files++
		bytes += b
	}
	return files, bytes, nil
}

// indexKeysUnder 返回索引里以 prefix+"/" 开头的键（排序，稳定输出顺序）。
func (s *SecretdataFS) indexKeysUnder(prefix string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for k := range s.index {
		if strings.HasPrefix(k, prefix+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// exportOneFile 解密单个文件并写到 dst（返回字节数）。
func (s *SecretdataFS) exportOneFile(ctx context.Context, key, dst string) (int64, int64, error) {
	rc, err := s.OpenRead(ctx, key)
	if err != nil {
		return 0, 0, fmt.Errorf("secretdata: 读取 %q 失败: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	f, ferr := os.Create(dst)
	if ferr != nil {
		return 0, 0, ferr
	}
	n, cerr := io.Copy(f, rc)
	if closeErr := f.Close(); cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		return n, 0, fmt.Errorf("secretdata: 写 %q 失败: %w", dst, cerr)
	}
	return int64(n), int64(n), nil
}

// sanitizeExportSegment 把路径片段安全化为单个目录/文件名（保持可读、避免穿越与同名覆盖）。
func sanitizeExportSegment(seg string) string {
	seg = strings.Trim(seg, "/")
	if seg == "" {
		return "_root_"
	}
	repl := strings.NewReplacer("/", "_", "\\", "_", string(os.PathSeparator), "_")
	out := repl.Replace(seg)
	out = strings.ReplaceAll(out, "..", "__")
	return out
}
