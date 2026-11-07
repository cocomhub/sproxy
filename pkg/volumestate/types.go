// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"path"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ObjectRef 是失效信号的最小单元（P1：卷是第一维）。
//
// Path 是**卷根相对路径**（与 `sync.FS` 键空间一致，首段即 owner，如
// `alice/user/docs/a.txt`）。之所以不在本层拆出 owner：`sync.FS` 的调用方本就以该
// 键空间工作，多拆一层只会增加两处键拼装的漂移面。
type ObjectRef struct {
	Volume string
	Path   string
}

// DirSnapshot 是一个目录的**直接子项**快照（list 的缓存单元）。
type DirSnapshot struct {
	Entries []syncpkg.Entry `json:"entries"`
	// Gen 是回源时卷自报的代际号（0 = 卷未实现 StateGeneration）。
	Gen uint64 `json:"gen"`
	// FetchedAt 回源时刻（UnixNano）；TTL 判定用。
	FetchedAt int64 `json:"fetched_at"`
	// Volume 归属校验（防串卷：磁盘快照被错误复用）。
	Volume string `json:"volume"`
}

// normalizePath 归一 FS 相对路径：去首尾斜杠、正斜杠、去空段。
// 保持与 sync.FS 契约一致（空串 = 卷根）。
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p == "" || p == "." {
		return ""
	}
	return path.Clean(p)
}

// parentPath 返回父目录（"" 的父仍是 ""——卷根不可再上溯）。
func parentPath(p string) string {
	p = normalizePath(p)
	if p == "" {
		return ""
	}
	d := path.Dir(p)
	if d == "." || d == "/" {
		return ""
	}
	return d
}

// ancestorPaths 返回从 p 的父目录到卷根的全部祖先（含空串根）。
// 失效时把祖先标记 stale：新增/删除子项会改变祖先目录视图。
func ancestorPaths(p string) []string {
	out := make([]string, 0, 8)
	for cur := parentPath(p); ; cur = parentPath(cur) {
		out = append(out, cur)
		if cur == "" {
			return out
		}
	}
}
