// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// root_fs.go —— 「卷根 → sync.FS」适配器的**别名**（实现已下移到 `pkg/rootfs`，M2a）。
//
// 为什么下移：`pkg/volume/registry`（`Set.FSFor`）与 `pkg/volume/enclosure`（`SourceFS`）需要同一
// 适配器来**关闭绕行点**（本地卷当前用裸 `LocalFS(RootDir)` ⇒ 加密卷读密文/写明文 + 绕过封印 ✗），
// 而它们不能 import `pkg/server`（成环 ✗）。见设计 `docs/designs/2026-10-16-quota-volume-layer.md` §11。
//
// 本别名保留全部 9 个既有调用点（零行为变更 ✓）；M3 收敛后删除。
func NewRootFS(root *storage.Root) syncpkg.FS { return rootfs.New(root) }
