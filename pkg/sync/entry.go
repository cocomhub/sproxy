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
	var out []Entry
	visited := make(map[string]bool)
	if err := walkDir(ctx, f, root, root, recursive, followSymlinks, filters, visited, 0, &out); err != nil {
		return nil, err
	}
	// 空（或全部被过滤掉）的根目录：作为一个空目录条目返回
	if len(out) == 0 {
		out = append(out, *rootEntry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func walkDir(ctx context.Context, f FS, root, dir string, recursive, followSymlinks bool, filters []Filter, visited map[string]bool, depth int, out *[]Entry) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("目录深度超限（疑似符号链接环）: %s", dir)
	}
	entries, err := f.ListDir(ctx, dir)
	if err != nil {
		return fmt.Errorf("列目录 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if isInternalName(e.Name) {
			continue
		}
		rel := stripRootPrefix(e.Path, root)
		if e.IsDir {
			// 目录：仅 exclude 命中才剪枝；include 不阻断递归——否则 path.Match 的 `*`
			// 不跨 `/`，`--include "*.go"` 会让 `sub/x.go` 所在子树被整棵遗漏（审查 I-1）。
			if !MatchFiltersDir(rel, filters) {
				continue
			}
			if recursive {
				before := len(*out)
				if err := walkDir(ctx, f, root, e.Path, recursive, followSymlinks, filters, visited, depth+1, out); err != nil {
					return err
				}
				// 空目录（或过滤后为空）：include 命中（或无 include）才作为目录条目输出
				if len(*out) == before && MatchFilters(rel, filters) {
					*out = append(*out, e)
				}
			} else if MatchFilters(rel, filters) {
				*out = append(*out, e)
			}
			continue
		}
		if !MatchFilters(rel, filters) {
			continue
		}
		if e.IsSymlink {
			if !followSymlinks {
				*out = append(*out, e)
				continue
			}
			resolved, rerr := f.Stat(ctx, e.Path)
			if rerr != nil || resolved == nil {
				// 损坏/无法解析的符号链接：保留为符号链接条目（引擎跳过）
				*out = append(*out, e)
				continue
			}
			if visited[e.Path] {
				continue // 环保护：同一符号链接路径只跟随一次
			}
			if resolved.IsDir {
				visited[e.Path] = true
				if recursive {
					if err := walkDir(ctx, f, root, e.Path, recursive, followSymlinks, filters, visited, depth+1, out); err != nil {
						return err
					}
				} else {
					*out = append(*out, *resolved)
				}
			} else {
				e2 := *resolved
				e2.IsSymlink = false
				*out = append(*out, e2)
			}
			continue
		}
		*out = append(*out, e) // 常规文件
	}
	return nil
}
