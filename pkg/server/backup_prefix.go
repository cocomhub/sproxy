// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// prefixFS 是 `syncpkg.PrefixFS` 的包内别名（**单一实现**：C1 修复把桶前缀视图从
// pkg/server 提升到 pkg/sync，供备份目标（本包）与 cmd/sproxy 的同步载体共用——
// 后者此前缺前缀视图，用户相对路径直达外部卷根，sidecar 落用户可见键空间）。
// 语义与透明装饰器契约（R21 全能力转发）见 pkg/sync/prefixfs.go。
type prefixFS = syncpkg.PrefixFS

// 编译期契约：桶前缀视图满足基础 FS 接口（构造点统一走 syncpkg.NewPrefixFS）。
var _ syncpkg.FS = (*prefixFS)(nil)
