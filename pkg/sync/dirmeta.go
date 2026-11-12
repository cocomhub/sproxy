// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// dirmeta.go 定义「目录 meta 残留清理」可选能力（FS 能力，与 RangeReader/WriteIfAbsent/
// StorageInspector 同族）。
//
// 为什么定义在 pkg/sync（2026-11-12，沿用 StorageInspector 于 #1096 下沉的同一判据）：
// 实现方在 `pkg/volume/secretdata`，而装饰链**中间层**（`pkg/sync` 的 PrefixFS /
// StagingQuotaGateFS、`pkg/volumestate`、`pkg/volume/...`）都要能引用并转发它。能力若留在
// 消费包（`pkg/server`）或 `pkg/volume`，`pkg/sync` 层的装饰器**无法引用** ⇒ 链条末端断言
// 恒 false ⇒ **静默降级**（详见 docs/decorator-capability-forwarding.md）。
//
// 未实现该能力的卷 ⇒ 转发层返回 `ErrUnsupported`（调用方按"未实现"处置），
// **不得**让断言失败后静默跳过。

// DirMetaCleaner 是「删除容器内非权威目录 meta」的可选能力（实现方：`secretdata` 的
// `SecretdataFS`）。
//
// 约定：
//   - **幂等** —— 无残留（含已清理干净）⇒ `(0, nil)`，因此可安全重复执行（重试语义）；
//   - 只删本卷自身的目录 meta blob，不读写用户内容；
//   - `container` 是**领域实现自身键空间内**的容器路径（上报方给出，装饰层只透传、不重写）。
type DirMetaCleaner interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	// CleanupStaleDirMeta 删除容器内非权威的目录 meta 残留，返回删除个数。
	CleanupStaleDirMeta(ctx context.Context, container string) (int, error)
}

// AssertDirMetaCleaner 探测卷视图是否实现目录 meta 清理能力（仓内既有「可选能力 +
// Assert*」模式）。
func AssertDirMetaCleaner(v any) (DirMetaCleaner, bool) {
	if v == nil {
		return nil, false
	}
	c, ok := v.(DirMetaCleaner) //nolint:forcetypeassert // 类型断言探测，ok 语义即能力存在性
	return c, ok
}
