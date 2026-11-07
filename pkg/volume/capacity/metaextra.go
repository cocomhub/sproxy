// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// metaextra.go 为容量装饰器**登记编译期能力断言**（R21）：`UpdateMetaExtra` 转发实现在
// capacity.go（委托 inner；底层未实现 → ErrUnsupported）。本文件单独放置断言，便于门禁
// 扫描（断言须位于 `var ( ... )` 块内，见 R21 匹配规则）。
var (
	_ syncpkg.MetaExtraUpdater = (*CapacityFS)(nil)
)
