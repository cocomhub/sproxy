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

// exportOneContainer 导出单个容器内的全部文件（临时注册 + 解密 + 写本地 + 全量回滚）。
// **同名隔离**：路径含「逻辑目录 + **落选容器标识** + 容器内相对路径 + 文件名」——
// 不同落选容器常有同名文件（如都叫 `X.mp4`），必须靠容器标识区分（用户明确要求）。
func (s *SecretdataFS) exportOneContainer(ctx context.Context, groupRel, containerRoot, container, suffix, dstRoot string) (int, int64, error) {
	reg := s.beginExportTempRegistration(container)
	defer reg.rollback()

	if err := s.loadContainerFileMetas(ctx, container, reg.tmpRel); err != nil {
		return 0, 0, err
	}
	reg.keysAdded = s.indexKeysUnder(reg.tmpRel) // 一次性收集：遍历 + 回滚共用（避免二次全索引扫描）

	// **同名隔离**：<dstRoot>/<逻辑目录>/<落选容器标识>/<容器内相对路径>/<文件名>
	outDir := filepath.Join(dstRoot,
		sanitizeExportSegment(groupRel),
		sanitizeExportSegment(path.Base(containerRoot)),
		sanitizeExportSegment(suffix))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, 0, err
	}
	files, bytes := 0, int64(0)
	for _, k := range reg.keysAdded {
		name := strings.TrimPrefix(k, reg.tmpRel+"/")
		n, werr := s.exportOneFile(ctx, k, filepath.Join(outDir, sanitizeExportSegment(name)))
		if werr != nil {
			return files, bytes, werr
		}
		files++
		bytes += n
	}
	return files, bytes, nil
}

// exportTempReg 是导出期间的**临时索引注册**及其**全量回滚**（批次 16 P1-4）。
//
// **为什么必须全量回滚**：`registerLoadedEntryLocked` 的副作用不止索引键——它还累加
// `s.usage`、经 `addDirKeysLocked` 写入**全部祖先目录键**、抬升 `s.volVersion`、并可能
// `dedupPool` 引用 +1。若只删索引前缀键与 `s.dirs[tmpRel]`，就会留下用量虚增、幻影目录
// （顶层 `__dup_export__` 会出现在 ListDir("/")）与永不回收的池引用。
// 导出是装配期/运维动作（无并发写者），故此处按**快照-还原**方式整体回滚。
type exportTempReg struct {
	s         *SecretdataFS
	tmpRel    string
	keysAdded []string

	prevSeg    string
	hadSeg     bool
	usage      int64
	version    int64
	dirsBefore map[string]struct{}
	poolBefore map[string]int64
}

// beginExportTempRegistration 建立临时注册并返回回滚句柄（调用方 defer rollback）。
func (s *SecretdataFS) beginExportTempRegistration(container string) *exportTempReg {
	r := &exportTempReg{s: s, tmpRel: dupExportTempPrefix + sanitizeExportSegment(container)}
	s.mu.Lock()
	r.prevSeg, r.hadSeg = s.dirSegs[r.tmpRel]
	s.dirSegs[r.tmpRel] = container
	s.dirs[r.tmpRel] = struct{}{}
	r.usage, r.version = s.usage, s.volVersion
	r.dirsBefore = make(map[string]struct{}, len(s.dirs))
	for k := range s.dirs {
		r.dirsBefore[k] = struct{}{}
	}
	r.poolBefore = make(map[string]int64, len(s.dedupPool))
	for k, v := range s.dedupPool {
		r.poolBefore[k] = v.refs
	}
	s.mu.Unlock()
	return r
}

// rollback 撤销临时注册的全部副作用（幂等：可安全重复调用）。
func (r *exportTempReg) rollback() {
	if r == nil {
		return
	}
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range r.keysAdded {
		delete(s.index, k)
	}
	if r.hadSeg {
		s.dirSegs[r.tmpRel] = r.prevSeg
	} else {
		delete(s.dirSegs, r.tmpRel)
	}
	for k := range s.dirs {
		if _, ok := r.dirsBefore[k]; !ok {
			delete(s.dirs, k)
		}
	}
	delete(s.dirs, r.tmpRel) // tmpRel 在快照前已加入 ⇒ 差集不含它，需显式删除
	s.usage = r.usage
	s.volVersion = r.version
	for k := range s.dedupPool {
		if _, ok := r.poolBefore[k]; !ok {
			delete(s.dedupPool, k)
		}
	}
	for k, refs := range r.poolBefore {
		if blk, ok := s.dedupPool[k]; ok {
			blk.refs = refs
		}
	}
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

// exportOneFile 解密单个文件并写到 dst（返回写入字节数）。
func (s *SecretdataFS) exportOneFile(ctx context.Context, key, dst string) (int64, error) {
	rc, err := s.OpenRead(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("secretdata: 读取 %q 失败: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	f, ferr := os.Create(dst)
	if ferr != nil {
		return 0, ferr
	}
	n, cerr := io.Copy(f, rc)
	if closeErr := f.Close(); cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		return n, fmt.Errorf("secretdata: 写 %q 失败: %w", dst, cerr)
	}
	return n, nil
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
