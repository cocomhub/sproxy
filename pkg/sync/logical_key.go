// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "github.com/cocomhub/sproxy/pkg/storage"

// logical_key.go —— `LogicalKey` 的**类型别名重导出**（统一存储卷模型片 4，设计
// `docs/designs/2026-10-16-quota-volume-layer.md` §9.5）。
//
// **定义在 `pkg/storage`**（**零仓内依赖**的叶子包）—— 完整文档见该包 `logical_key.go`。
//
// # 为何是类型别名（而非新类型/兼容层）
//
// `type LogicalKey = storage.LogicalKey` ⇒ **同一个类型** ⇒ **不是兼容层**：
// 无第二个定义、无需转换、无双重语义。调用点可继续写 `sync.LogicalKey`（`pkg/sync` 是
// `sync.FS` 的家，从它取该类型最自然）。
//
// # 🔴 不可改回独立定义
//
// 把本别名换回 `type LogicalKey struct{...}` 会**同时**引入两个问题：
//  1. 出现两个不同的类型（`sync.LogicalKey` ≠ `storage.LogicalKey`）⇒ 装配处需转换
//     （编译器会以 `cannot use storage.LogicalKey{} ... as sync.LogicalKey value` 报出）；
//  2. `pkg/files/meta` 及更下层包**无法引用** `pkg/sync`（import 环）⇒ 签名迁移必然失败。
//
// 第 2 点即 2026-10-17 迁移多次受挫的根因实证：`pkg/backup → pkg/sync → pkg/files/meta
// → pkg/volume → pkg/storage`，而 `LogicalKey` 必须能被 `pkg/files/meta` 之下的包引用。
type LogicalKey = storage.LogicalKey
