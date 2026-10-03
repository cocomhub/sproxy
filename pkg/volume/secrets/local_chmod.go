// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"os"
	"path/filepath"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// local_chmod.go 实现 secrets 卷的本地 0600 权限收紧：底层为 sync.LocalFS 时，
// 复用其根目录（Root 字段）定位绝对路径后 chmod——LocalFS 自身只暴露 FS 接口，
// 权限位不参与同步语义（sync.Engine 不关心 perms），故在此按绝对路径补 chmod。
func chmodPath(lfs *syncpkg.LocalFS, name string) error {
	full := filepath.Join(lfs.Root, filepath.FromSlash(name))
	return os.Chmod(full, 0o600)
}
