// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volume_local_backend.go —— 本地卷的**统一后端实现**（统一存储卷模型 M1）。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。
//
// 现状（本文件要消除的特例）：本地卷**不是**注册后端 —— 装配层以 `Type == "" || TypeLocal`
// 走 `assembleLocalVolume` 特例分支，`*storage.Root` 因此泄漏到非测试源码 **254 处**；
// 而外部卷早已统一（`registry.ExternalBackend` = `FS() + Close()`）。
//
// 本片（M1a）只提供**实现**，**不注册**（`registry.RegisterBackend(volume.TypeLocal, …)` 由
// 下一片连同 2 个 API 面守卫一起做）⇒ **零行为变更** ✓：
//   - `NewBackend` 自身仍守卫 local（`registry/backend.go`）⇒ 无调用点行为改变 ✓；
//   - `BackendSchemas`/`IsBackendRegistered` 仍不含 local ⇒ backend 列表 API 不变 ✓。
//
// **为什么不做第二份实现**：`OpenLocalVolumeRoot`（MkdirAll + `storage.OpenRoot` + at-rest
// 加密 key 解析/cipher 校验，fail-closed）**已是**本地卷根开启的唯一实现 ⇒ 本后端**复用它** ✓
// （禁复制加密/布局逻辑 ✗）；`NewRootFS` 同理（卷根 → `sync.FS` 的唯一适配器 ✓）。
//
// **命名注记**（用户裁定 2026-10-16）：`ExternalBackend` 是**过时命名** —— 整合完成后允许
// **破坏性变更**统一命名（`ExternalBackend` → `StorageVolume` 等），届时不得为兼容而保留双名 ✗。
type localBackend struct {
	name string
	root *storage.Root
	fs   syncpkg.FS
}

// NewLocalBackend 构造本地卷后端：复用 `OpenLocalVolumeRoot`（含 at-rest 加密与 fail-closed
// 语义）⇒ 与装配层**同一实现**。失败时已关闭本根（`OpenLocalVolumeRoot` 保证 ✓）。
func NewLocalBackend(v volume.Volume, rootDir string, log *slog.Logger) (*localBackend, error) {
	if log == nil {
		log = slog.Default()
	}
	root, err := OpenLocalVolumeRoot(v.Name, rootDir, v.Extra, log)
	if err != nil {
		return nil, err
	}
	return &localBackend{name: v.Name, root: root, fs: NewRootFS(root)}, nil
}

// Name 返回卷名（装配期唯一）。
func (b *localBackend) Name() string { return b.name }

// Kind 返回结构性类型：本地卷**承载真实字节** ⇒ `KindStorage`（需配额层 ✓）。
func (b *localBackend) Kind() volume.Kind { return volume.KindStorage }

// FS 返回卷根相对键空间的数据面视图（装配后只读：同一实例）。
func (b *localBackend) FS() syncpkg.FS { return b.fs }

// Close 关闭卷根（幂等由 `storage.Root.Close` 保证）。
func (b *localBackend) Close() error { return b.root.Close() }

// Root 暴露底层卷根（**过渡期**：装配层/路由仍以 `*storage.Root` 访问；M3 收敛后删除 ✗）。
func (b *localBackend) Root() *storage.Root { return b.root }

// 编译期：本地卷后端**同时**满足外部后端契约与统一存储卷契约 —— 这是「`local` 可作为普通
// 后端注册」的前提（M1 注册片 + M3 迁移片都依赖它）。
var (
	_ registry.ExternalBackend = (*localBackend)(nil)
	_ registry.StorageVolume   = (*localBackend)(nil)
)
