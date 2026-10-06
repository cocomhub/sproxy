// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"errors"
	"strings"
)

// 嵌套封装 target 语法（用户 2026-10-06 确认的最终语义）：
//
//	`<卷名>/<新子目录>`（如 `main/videos`），引用已创建卷的**新空子目录**作为封装根，
//	**不新建底层卷**。子目录必须不存在（防与底层卷既有数据混合），且同卷路径重叠
//	（父/子/同目录）互斥占用。
//
// 历史路径 `target: local + extra.root`（config/CLI）为本地根整卷，不走嵌套语义（零回归）。

// ErrNestedTarget 是嵌套封装 target 形态非法时的哨兵（缺子目录/越界）。
var ErrNestedTarget = errors.New("volume: 嵌套封装 target 需 <卷>/<新子目录> 形态")

// SplitNestedTarget 解析嵌套封装 target 语法 `<卷名>/<新子目录>`：
// 返回 baseVol（卷名）+ subdir（子目录，含多级 `a/b`，去首尾 `/`）。不满足（无分隔、
// 卷名空、子目录空）→ ok=false（调用方按非嵌套 target 处理）。
func SplitNestedTarget(target string) (base, subdir string, ok bool) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", false
	}
	i := strings.IndexByte(t, '/')
	if i <= 0 || i == len(t)-1 {
		return "", "", false
	}
	base = t[:i]
	subdir = strings.Trim(t[i+1:], "/")
	if base == "" || subdir == "" {
		return "", "", false
	}
	return base, subdir, true
}

// PathsOverlap 判定两个底层子目录路径是否重叠（互斥占用冲突）：
//
//	a==b，或 b 是 a 的子目录（b 以 "a/" 开头），或 a 是 b 的子目录（a 以 "b/" 开头）。
//
// 同卷不同分支（`main/photos` vs `main/movies`）不重叠；跨卷天然互不影响。
func PathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}
