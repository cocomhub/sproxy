// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"fmt"
	"strings"
)

// Scheme 是跨节点卷访问句柄的 URL scheme。
const Scheme = "remote"

// Ref 是一个跨节点卷访问句柄：`remote://<node>/<vol>/<path>`。
//
// 三个字段都可独立使用：`Node` + `Volume` 足以定位一个卷（`Client.FS` 只取这两个），
// 加上 `Path` 即定位卷内一个条目。**`Path` 是卷内相对路径**（正斜杠、无前导斜杠），
// 与 `sync.FS` 的路径契约一致——`String()` 与 `ParseRef` 往返一致。
type Ref struct {
	Node   string
	Volume string
	Path   string
}

// ParseRef 解析 `remote://<node>/<vol>[/<path>]`。
//
// 拒绝（全部 fail-closed，不猜测）：
//   - 非 `remote://` 前缀；
//   - 缺节点（`remote:///vol/x`）或缺卷（`remote://node`）；
//   - 路径含 `.` / `..` 段或反斜杠（归一为分隔符后同一判定）。
//
// 允许：路径含重复斜杠与尾斜杠（归一）；`remote://nodeB/main`（卷根，Path 为空）。
func ParseRef(s string) (Ref, error) {
	trimmed := strings.TrimSpace(s)
	rest, ok := strings.CutPrefix(trimmed, Scheme+"://")
	if !ok {
		return Ref{}, fmt.Errorf("remote: 句柄必须以 %s:// 开头: %q", Scheme, s)
	}
	node, tail, _ := strings.Cut(rest, "/")
	vol, rawPath, _ := strings.Cut(tail, "/")
	if node == "" {
		return Ref{}, fmt.Errorf("remote: 句柄缺少节点名: %q", s)
	}
	if vol == "" {
		return Ref{}, fmt.Errorf("remote: 句柄缺少卷名（形如 %s://<node>/<vol>/<path>）: %q", Scheme, s)
	}
	path, err := normalizeRelPath(rawPath)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Node: node, Volume: vol, Path: path}, nil
}

// String 把 Ref 还原为句柄（`ParseRef(String())` 与原值等价）。
func (r Ref) String() string {
	s := Scheme + "://" + r.Node + "/" + r.Volume
	if p, err := normalizeRelPath(r.Path); err == nil && p != "" {
		s += "/" + p
	}
	return s
}

// Root 返回只含节点与卷的 Ref（卷根；供 `Client.FS` 使用）。
func (r Ref) Root() Ref { return Ref{Node: r.Node, Volume: r.Volume} }

// IsRoot 报告该句柄是否指向卷根（无路径）。
func (r Ref) IsRoot() bool {
	p, err := normalizeRelPath(r.Path)
	return err == nil && p == ""
}
