// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import "strings"

// NormalizeRemote 归一协议路径：/ 为唯一协议分隔符（跨平台反斜杠视为分隔符）；
// TrimSpace、拒绝空、绝对路径、Windows 卷名、. / .. / 空段；返回 ToSlash 形式路径。
func NormalizeRemote(remotePath string) (string, bool) {
	norm := strings.TrimSpace(remotePath)
	if norm == "" {
		return "", false
	}
	// 统一分隔符：反斜杠视为分隔符（Windows 输入兼容）。
	norm = strings.ReplaceAll(norm, `\`, "/")
	if strings.HasPrefix(norm, "/") {
		return "", false
	}
	// Windows 卷名（C:/x、C:foo）视为绝对路径。
	if len(norm) >= 2 && norm[1] == ':' {
		return "", false
	}
	for seg := range strings.SplitSeq(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return norm, true
}

// validSegments 逐段校验协议路径（/ 分隔）：所有段均通过 ValidSegmentName 才返回 true。
// 供 UserRel / FeatureRel 复用，保证段名校验单一权威在两个单入口都生效。
func validSegments(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if !ValidSegmentName(seg) {
			return false
		}
	}
	return true
}

// JoinRel 用 / 拼接协议路径段。
func JoinRel(segs ...string) string {
	return strings.Join(segs, "/")
}
