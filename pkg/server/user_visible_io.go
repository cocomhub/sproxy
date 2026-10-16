// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// user_visible_io.go —— 用户内容读写**唯一入口**的薄包装（实现已下移到 `pkg/rootfs`，M2a）。
//
// 为什么下移：`pkg/volume/registry`（`Set.FSFor`）与 `pkg/volume/enclosure`（`SourceFS`）需要同一
// 入口来**关闭绕行点**（本地卷当前用裸 `LocalFS(RootDir)` ⇒ 加密卷读密文/写明文 + 绕过封印 ✗），
// 而它们不能 import `pkg/server`（成环 ✗）。见设计 `docs/designs/2026-10-16-quota-volume-layer.md` §11。
//
// **薄包装而非改调用点**：48 个调用点保持原名字 ⇒ 零改动、零行为变更 ✓（纯委托，同一实现）。
// R25 门禁的白名单同步指向新位置（`pkg/rootfs/visible_io.go` = 唯一入口定义 ✓）。

func openUserVisibleRead(root *storage.Root, rel string) (io.ReadCloser, error) {
	return rootfs.OpenUserVisibleRead(root, rel)
}

func writeUserVisible(root *storage.Root, rel string, flag int, perm os.FileMode) (io.WriteCloser, error) {
	return rootfs.WriteUserVisible(root, rel, flag, perm)
}

func userVisibleSize(root *storage.Root, rel string) (int64, error) {
	return rootfs.UserVisibleSize(root, rel)
}

func relDir(rel string) string { return rootfs.RelDir(rel) }

func ensureDir(root *storage.Root, rel string, perm os.FileMode) error {
	return rootfs.EnsureDir(root, rel, perm)
}

func relJoin(rel, name string) string { return rootfs.RelJoin(rel, name) }
