// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Entry 表示一个文件系统条目。
type Entry struct {
	Name      string // 文件名（Path 最后一段）
	Path      string // 相对路径（正斜杠，不含根）
	Size      int64
	MTime     int64  // UnixNano
	Checksum  string // SHA-256 hex；空=未知（调用方按需计算）
	IsDir     bool
	IsSymlink bool
}

// FS 抽象文件系统操作，供 LocalFS 与后续远程传输实现。
//
// 所有 path 参数均为「FS 根相对的相对路径」，正斜杠分隔。
// Stat 对不存在的路径返回 (nil, nil)；ListDir 返回条目的 Path 为完整相对路径。
//
// Path 契约（审查 R3）：所有实现必须返回「FS 根相对的相对路径」（正斜杠、无根前缀、
// 无盘符/绝对路径）——Engine 用 stripRootPrefix/joinSlash 做 src↔dst 路径映射，
// conflict_rename 的 RenameDstTo 也基于此生成；HTTPTransport 必须保证 Stat/ListDir
// 与 LocalFS 返回一致格式，否则重命名会错位到绝对/带前缀路径。
type FS interface {
	ListDir(ctx context.Context, path string) ([]Entry, error)
	Stat(ctx context.Context, path string) (*Entry, error)
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, path string, r io.Reader, size int64, mtime int64) error
	Rename(ctx context.Context, from, to string) error
	Delete(ctx context.Context, path string) error
	MakeDir(ctx context.Context, path string) error
}

// WriteIfAbsent 是 FS 的可选原子写能力（并发安全前提）：
// 仅当 path 在目标处不存在时写入并返回 (true, nil)；若已存在则**不覆盖**并返回
// (false, nil)。用于「转存目标须唯一、拒绝静默覆盖」类语义（W1/W3）。
//
// 使用方（如 pkg/cloud 转存）通过类型断言查询该能力，而**不**在调用侧硬编码每类卷的
// 存在性语义（机制化：卷自描述能力，使用方只查能力→查实现）。
type WriteIfAbsent interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	WriteIfAbsent(ctx context.Context, path string, r io.Reader, size int64, mtime int64) (bool, error)
}

// ReserveSpace 是 FS 的可选容量预检能力（卷自管理配额，NH1）：
// 卷后端实现则转存前预分配空间检查——成功返回 nil；不足返回 error（fail-closed 不转存）。
// 未实现 = 卷无独立配额概念（由用户配额/全局账本管），转存侧跳过卷配额检查。
//
// 机制化：使用方（pkg/cloud 转存）只查询该能力，不在调用侧硬编码每类卷的容量语义；
// 外部网盘卷（s3/baidupcs 等）在各自后端实现（有配额 API），本地/加密卷可依赖
// Volume.Capacity（装配层）或全局账本。
type ReserveSpace interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	ReserveSpace(ctx context.Context, relPath string, size int64) error
}

// LocalVolume 是 FS 的**本地性自述能力**（用户裁定 2026-10-05）：
//   - **未实现该接口的 FS 默认视为外部卷**（false）——registry 域内注册的都是外部卷
//     后端，外部新增零配置自动扩展；**内部/本地卷必须实现** `IsLocalVolume()==true`，
//     以显式声明自己是内部卷（走用户配额 + 提供本地化便利）；
//   - **封装卷**（secretdata/secrets 等 wrapper）实现时委派给被封装的底层卷——「只要
//     有一层是外部就是外部」，层层判断，不靠类型名硬编码（secretdata 封装 s3 → 外部、
//     封装本地 → 内部自动正确）；
//   - 使用方（pkg/cloud 转存配额）经类型断言查询：实现且返回 true → 内部/本地卷（用户
//     配额生效）；否则 → 外部卷（容量/配额由卷自身管理，用户通用配额跳过）。
type LocalVolume interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	IsLocalVolume() bool
}

// maxWalkDepth 限制目录递归深度（符号链接环的 fail-closed 兜底）。
// 合法超深目录（>128 层）会因此被误判为疑似环而报错；对绝大多数真实目录树足够，
// 且环检测比放任无限递归更安全（审查 M11：保持 fail-closed）。
const maxWalkDepth = 128

// joinSlash 用正斜杠拼接 base 与 rel。
func joinSlash(base, rel string) string {
	if base == "" {
		return rel
	}
	if rel == "" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(rel, "/")
}

// stripRootPrefix 移除路径的 root 前缀，返回相对 root 的子路径。
// path == root 时返回 ""；path 不以 root 开头时原样返回。
func stripRootPrefix(p, root string) string {
	if root == "" || root == "." {
		return p
	}
	if p == root {
		return ""
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if after, ok := strings.CutPrefix(p, prefix); ok {
		return after
	}
	return p
}

// isInternalName 判断是否为内部元数据目录（以 .__ 开头），对齐服务端 .__* 内部目录约定。
func isInternalName(name string) bool {
	return strings.HasPrefix(name, ".__")
}

// WalkEntries 递归（或单层）枚举 fs 中 root 下的源树，返回满足过滤的条目。
//
//   - root 为 FS 根相对的路径（"" 表示整个根）。返回条目的 Path 为 FS 根相对的完整相对路径，
//     可直接用于 FS 的 Stat/OpenRead 等方法。
//   - 过滤与内部目录（.__ 前缀）跳过在遍历时应用；过滤器匹配路径为「相对 root」的子路径。
//   - recursive=false 时只枚举 root 顶层，子目录以目录条目形式返回（不递归）。
//   - 符号链接：followSymlinks=false 时以 IsSymlink=true 的条目返回（由调用方决定跳过）；
//     true 时解析目标并入树（目录递归、文件按内容），自环/增长环由深度上限 fail-closed。
func WalkEntries(ctx context.Context, f FS, root string, recursive, followSymlinks bool, filters []Filter) ([]Entry, error) {
	rootEntry, err := f.Stat(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("stat 源路径 %q 失败: %w", root, err)
	}
	if rootEntry == nil {
		return nil, fmt.Errorf("源路径不存在: %s", root)
	}
	if !rootEntry.IsDir {
		return []Entry{*rootEntry}, nil
	}
	// walker 承载一次 WalkEntries 遍历的共享过滤/递归上下文（S107：收敛 walkDir 家族
	// 10+ 参数为结构体；ctx/f/root/recursive/followSymlinks/filters/visited/out 全程不变）。
	w := &walker{ctx: ctx, f: f, root: root, recursive: recursive, followSymlinks: followSymlinks, filters: filters, visited: make(map[string]bool)}
	if err := w.walkDir(root, 0); err != nil {
		return nil, err
	}
	// 空（或全部被过滤掉）的根目录：作为一个空目录条目返回
	if len(w.out) == 0 {
		w.out = append(w.out, *rootEntry)
	}
	sort.Slice(w.out, func(i, j int) bool { return w.out[i].Path < w.out[j].Path })
	return w.out, nil
}

// walker 是 WalkEntries 递归遍历共享的工作上下文（S107：收敛 walkDir/walkDirEntry/
// walkDirSubtree/appendSymlinkEntry 的 ctx/f/root/recursive/followSymlinks/filters/
// visited/out 参数为结构体）。
type walker struct {
	//nolint:containedctx // S8242 已评估：单次目录遍历共享 ctx（S107 收敛），非请求侧驻留——与下行 NOSONAR 双抑制
	ctx            context.Context // NOSONAR: S8242 — 单次目录遍历的共享 ctx（S107 收敛），非请求侧驻留
	f              FS
	root           string
	recursive      bool
	followSymlinks bool
	filters        []Filter
	visited        map[string]bool
	out            []Entry
}

// walkDir 递归（或单层）枚举 dir，深度超限（疑似符号链接环）时 fail-closed 报错。
func (w *walker) walkDir(dir string, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("目录深度超限（疑似符号链接环）: %s", dir)
	}
	entries, err := w.f.ListDir(w.ctx, dir)
	if err != nil {
		return fmt.Errorf("列目录 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if err := w.walkDirEntry(e, depth); err != nil {
			return err
		}
	}
	return nil
}

// walkDirEntry 处理单个目录条目：内部目录跳过、目录走子树剪枝/递归、文件走过滤与符号链接处理。
func (w *walker) walkDirEntry(e Entry, depth int) error {
	if isInternalName(e.Name) {
		return nil
	}
	rel := stripRootPrefix(e.Path, w.root)
	if e.IsDir {
		return w.walkDirSubtree(e, rel, depth)
	}
	if !MatchFilters(rel, w.filters) {
		return nil
	}
	if e.IsSymlink {
		return w.appendSymlinkEntry(e, depth)
	}
	w.out = append(w.out, e) // 常规文件
	return nil
}

// walkDirSubtree 处理目录条目：仅 exclude 命中才剪枝；include 不阻断递归——否则
// path.Match 的 `*` 不跨 `/`，`--include "*.go"` 会让 `sub/x.go` 所在子树被整棵遗漏
// （审查 I-1）。递归后空目录（或过滤后为空）在 include 命中（或无 include）时作为目录条目输出。
func (w *walker) walkDirSubtree(e Entry, rel string, depth int) error {
	if !MatchFiltersDir(rel, w.filters) {
		return nil
	}
	if w.recursive {
		before := len(w.out)
		if err := w.walkDir(e.Path, depth+1); err != nil {
			return err
		}
		if len(w.out) == before && MatchFilters(rel, w.filters) {
			w.out = append(w.out, e)
		}
		return nil
	}
	if MatchFilters(rel, w.filters) {
		w.out = append(w.out, e)
	}
	return nil
}

// appendSymlinkEntry 处理符号链接条目：不跟随 → 原样输出；跟随 → 解析目标，
// 目录递归进入、文件以解析后元信息输出；环保护（同一路径只跟随一次）。
func (w *walker) appendSymlinkEntry(e Entry, depth int) error {
	if !w.followSymlinks {
		w.out = append(w.out, e)
		return nil
	}
	resolved, rerr := w.f.Stat(w.ctx, e.Path)
	if rerr != nil || resolved == nil {
		// 损坏/无法解析的符号链接：保留为符号链接条目（引擎跳过）
		w.out = append(w.out, e)
		return nil
	}
	if w.visited[e.Path] {
		return nil // 环保护：同一符号链接路径只跟随一次
	}
	if resolved.IsDir {
		w.visited[e.Path] = true
		if w.recursive {
			if err := w.walkDir(e.Path, depth+1); err != nil {
				return err
			}
		} else {
			w.out = append(w.out, *resolved)
		}
	} else {
		e2 := *resolved
		e2.IsSymlink = false
		w.out = append(w.out, e2)
	}
	return nil
}
