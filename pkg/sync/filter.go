// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"path"
	"strings"
)

// ParseFilters 将 include/exclude 模式列表解析为 Filter 切片。
// include 模式 Exclude=false，exclude 模式 Exclude=true；空（或纯空白）pattern 跳过。
func ParseFilters(include, exclude []string) []Filter {
	filters := make([]Filter, 0, len(include)+len(exclude))
	for _, p := range include {
		if strings.TrimSpace(p) == "" {
			continue
		}
		filters = append(filters, Filter{Pattern: p})
	}
	for _, p := range exclude {
		if strings.TrimSpace(p) == "" {
			continue
		}
		filters = append(filters, Filter{Pattern: p, Exclude: true})
	}
	return filters
}

// MatchFilters 按 include/exclude 语义匹配 path（path.Match glob）。
//
// 语义：pattern 用 matchPattern 匹配（不含分隔符的 pattern 同时匹配完整路径与
// basename——rsync 风格，`--include "*.go"` 递归包含任意层级的 .go 文件，修复审查
// I-1）；Exclude 命中 → false；Include 命中 → true；无 include 过滤器时全部放行；
// exclude 优先于 include。空 pattern 跳过；非法 pattern 视为不匹配。
//
// 注意：本函数用于**叶子条目**（文件/符号链接）的完整判定；目录条目请用
// MatchFiltersDir（只判 exclude），否则 include 不匹配目录会把整棵子树剪枝。
func MatchFilters(p string, filters []Filter) bool {
	hasInclude := false
	includeHit := false
	for _, f := range filters {
		if strings.TrimSpace(f.Pattern) == "" {
			continue
		}
		ok, valid := matchPattern(f.Pattern, p)
		if !valid {
			continue // 非法 pattern 忽略（不产生 include 约束）
		}
		if f.Exclude {
			if ok {
				return false
			}
		} else {
			hasInclude = true
			if ok {
				includeHit = true
			}
		}
	}
	if !hasInclude {
		return true
	}
	return includeHit
}

// matchPattern 判断单个 glob pattern 是否匹配路径 p，返回 (是否匹配, pattern 是否合法)。
//
// 匹配规则：
//   - 先对完整相对路径做 path.Match；
//   - pattern 不含 `/` 时再对 basename 匹配（rsync 风格：`*.go` 匹配任意层级的
//     `x.go`；`sub/*.go` 这类含分隔符的 pattern 保持完整路径匹配）。
//
// path.Match 的 `*` 不跨 `/`，仅完整路径匹配会让 `--include "*.go"` 漏掉子目录
// 文件（审查 I-1），故按「无分隔符 pattern 匹配 basename」扩展。非法 pattern（如
// 不闭合的 `[`）返回 valid=false，调用方应忽略（对齐 path.Match 原生"非法视为不匹配"）。
func matchPattern(pattern, p string) (ok, valid bool) {
	m, err := path.Match(pattern, p)
	if err != nil {
		if !strings.Contains(pattern, "/") {
			if mb, berr := path.Match(pattern, path.Base(p)); berr == nil {
				return mb, true
			}
		}
		return false, false
	}
	if m {
		return true, true
	}
	if !strings.Contains(pattern, "/") {
		mb, _ := path.Match(pattern, path.Base(p))
		return mb, true
	}
	return false, true
}

// MatchFiltersDir 判断目录条目是否应继续递归/输出。
//
// 与 MatchFilters 不同：目录条目**只受 exclude 约束**（命中 exclude → false 剪枝，
// 其内容一并排除）；include 不阻断目录遍历——否则 `--include "*.go"` 这类叶子
// pattern 会因目录名不匹配而把整棵子树剪掉，深层匹配文件被静默遗漏。
// include 对目录的唯一作用是在该目录作为空目录输出时走 MatchFilters 完整判定。
func MatchFiltersDir(p string, filters []Filter) bool {
	for _, f := range filters {
		if !f.Exclude || strings.TrimSpace(f.Pattern) == "" {
			continue
		}
		if ok, valid := matchPattern(f.Pattern, p); valid && ok {
			return false
		}
	}
	return true
}
