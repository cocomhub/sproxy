// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume.go 是百度网盘作为「外部卷」的构造器。
//
// 定位：pkg/volume 是**纯域模型**（卷定义/ACL/选卷，无 I/O）；pkg/volume/registry
// 是运行时卷集合（根句柄 + 容量池）。baidupcs 卷 = StorageFS（网盘同步视图）+
// 本地中间态目录（RootDir，staging/cache 落它之下）——装配层（pkg/server）把
// 本构造器产出的「网盘卷描述」并入现有 registry 装配。
//
// 寻址：装配后 `baidupcs://<卷名>/<path>` 可被 syncmgr.Job 使用（src/dst 字符串）。
package baidupcs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// VolumeBackend 是百度网盘卷的运行时描述：FS 视图 + 本地中间态根目录。
type VolumeBackend struct {
	// Name 是卷名（装配进 registry 后与本地卷同名语义）。
	Name string
	// FS 是网盘同步视图（StorageFS）。
	FS syncpkg.FS
	// RootDir 是本地中间态基目录（staging/cache 落它之下；符合 volume.RootDir 契约）。
	RootDir string
}

// VolumeBackendConfig 是构造 baidupcs 卷的配置。
type VolumeBackendConfig struct {
	// Name 是卷名（必填）。
	Name string
	// Storage 是网盘 Storage（必填）。
	Storage StorageAPI
	// LocalRoot 是本地中间态基目录；空 = <os.TempDir()>/baidupcs/<Name>。
	LocalRoot string
}

// NewVolumeBackend 构造百度网盘卷（StorageFS + 本地布局）。
// 装配层可用 Name 生成 volume.Volume{RootDir: LocalRoot} 并入 registry。
func NewVolumeBackend(ctx context.Context, cfg VolumeBackendConfig) (*VolumeBackend, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("%w: volume name is empty", ErrInvalidParam)
	}
	if cfg.Storage == nil {
		return nil, fmt.Errorf("%w: storage is nil", ErrInvalidParam)
	}
	local := cfg.LocalRoot
	if local == "" {
		local = filepath.Join(os.TempDir(), "baidupcs", cfg.Name)
	}
	fs, err := NewStorageFS(cfg.Storage, local)
	if err != nil {
		return nil, err
	}
	return &VolumeBackend{Name: cfg.Name, FS: fs, RootDir: local}, nil
}

// _ 编译期断言：VolumeBackend.FS 是 sync.FS。
var _ syncpkg.FS = (*StorageFS)(nil)
