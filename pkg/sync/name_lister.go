// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// NameLister 是 ListDir 的**轻量列举**可选能力：只保证 Name/IsDir 正确，
// Size/ModTime 可能是底层 `ls` 的近似值（不逐条补齐精确元数据）。
//
// 用途：装配期只需要「名字 + 是否目录」的场景（如 secretdata 卷重建内存索引时按名字
// 区分 dir-meta / file-meta / 分块）。外部卷逐条补精确 size 的成本极高——baidupcs 每
// 30 条执行一次 `BaiduPCS-Go meta` CLI，1.2 万 blob ⇒ 数百次外部调用，实测触发网盘限流
// 31034，导致卷装配 fail-closed、服务无法启动。
//
// 消费方必须只依赖 Name/IsDir；需要精确 size 时走 ListDir。
type NameLister interface {
	ListDirNames(ctx context.Context, relPath string) ([]Entry, error) // NOSONAR: S8196 — 能力接口，设计保留
}

// AssertNameLister 查询文件系统是否提供轻量列举能力（未实现 ⇒ ok=false，调用方回落 ListDir）。
func AssertNameLister(fsys FS) (NameLister, bool) {
	nl, ok := fsys.(NameLister)
	return nl, ok
}
