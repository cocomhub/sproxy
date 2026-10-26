// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// files_meta_extra_source.go 为 files.Service 提供「按卷取旁路 Extra 更新能力」的装配实现。
//
// 能力链：卷的 FS（registry.Set.External(name).FS()）→ sync.AssertMetaExtraUpdater
// → trusted/secretdata/capacity 等装饰器的 UpdateMetaExtra（唯一实现）。
// 未装配/无能力 → nil（files 侧返回 ErrMetaUnavailable，fail-closed）。

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// metaExtraUpdaterAdapter 是 files.metaExtraUpdater 的适配（闭包持有底层 FS）。
type serverMetaExtraUpdater struct {
	fs   syncpkg.FS
	base string // 卷内用户可见相对路径前缀（user/，由 FS 键空间决定）
}

// UpdateMetaExtra 把用户可见 rel 映射到 FS 键后委托 UpdateMetaExtra。
func (u serverMetaExtraUpdater) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := syncpkg.AssertMetaExtraUpdater(u.fs)
	if !ok {
		return syncpkg.ErrUnsupported
	}
	return ue.UpdateMetaExtra(ctx, u.base+rel, extra)
}

// MetaExtraUpdater 实现 files.metaExtraUpdaterSource：按卷名取旁路更新能力。
func (a filesVolumeSet) MetaExtraUpdater(volName, _ string) files.MetaExtraUpdater {
	if a.Set == nil {
		return nil
	}
	be := a.External(volName)
	if be == nil {
		return nil
	}
	fsys := be.FS()
	if fsys == nil {
		return nil
	}
	if _, ok := syncpkg.AssertMetaExtraUpdater(fsys); !ok {
		return nil
	}
	return serverMetaExtraUpdater{fs: fsys, base: "user/"}
}
