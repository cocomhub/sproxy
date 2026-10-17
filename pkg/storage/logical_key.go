// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// logical_key.go —— `LogicalKey`：**配额归属键**（统一存储卷模型片 4，设计
// `docs/designs/2026-10-16-quota-volume-layer.md` §9.5）。
//
// # 为什么需要它
//
// 配额层（`registry.QuotaVolume`）包在**存储卷**上 ⇒ 到达它的写入路径已是**封装卷内部名**
// （secretdata 的随机容器名 + 内容哈希 ✗）⇒ **无法从路径归属到 (owner, bucket/rel)** ✗。
// 故归属必须由**上层显式下传** ✓。
//
// # 形状：`{Owner, Rel}` —— **只有 `owner` 是真正丢失的信息**
//
// 客户端可达路径恒为 `<卷根>/<owner>/<bucket>/<rel>`（命名空间契约）⇒ 路径参数**已经携带**
// `bucket/rel` ✓；封装卷内部名丢掉的**只是 `owner`** ✗ —— 但它**同时丢失了「原 `rel` 用于
// 作用域最长前缀解析」的能力** ✗（配额按功能桶/子目录最长前缀定 Scope ✓）⇒ 两者都要 ✓。
//
// ⇒ `LogicalKey` 就是**写内核已有的 `(owner, remotePath)` 对**
// （`files.Service.settleSize(route, owner, remotePath, written)` ✓）—— **不是新概念**，
// 而是把这个对**从写内核下传到卷内** ✓。
//
// **为何不用单一字符串键** ✗：字符串需重新解析，而「拼回去再拆开」正是归属漂移的经典根因 ✗；
// 结构化两字段让**非法状态不可表示** ✓（与仓内 `Location`/`Ref` 一致 ✓）。
//
// # 🔴 **为什么定义在 `pkg/storage`（而非 `pkg/sync`）** —— 2026-10-17 迁移实证 ✓
//
// 首次实现把本类型放在 `pkg/sync` ✗，导致签名迁移**必然撞 import 环** ✗：
//
//	pkg/backup → pkg/sync → pkg/files/meta → pkg/volume → pkg/storage
//	              ↑___________________________|
//
// 原因：`pkg/sync` **已 import** `pkg/files/meta`（`prefixfs.go` ✓），而**部分包不能 import
// `pkg/sync`**（会成环 ✗）—— 但它们**可以** import `pkg/storage` ✓（**零仓内依赖** ✓）。
// ⇒ 归属键必须放在**比 `pkg/files/meta` 更低的叶子包** ✓。
//
// `pkg/sync` 以**类型别名**重导出（`type LogicalKey = storage.LogicalKey` ✓）⇒
// **不是兼容层** ✗（同一类型 ✓，无第二个定义 ✓），调用点写法（`sync.LogicalKey`）保持不变 ✓。
type LogicalKey struct {
	// Owner 是**归属用户**（归一化前的原始值；消费方按各自契约归一，避免双口径）。
	Owner string
	// Rel 是**用户可见相对路径**（含功能桶前缀，如 `user/dir/f.txt`）——用于作用域最长前缀解析。
	Rel string
}

// IsZero 报告键是否为**零值**（= 无归属声明）。
//
// **语义（必须显式处置，禁猜 ✗）**：零值 = 调用方**未声明归属** ⇒ 配额层**不得**按路径反推
// （封装卷路径不可归属 ✗）⇒ 应**显式不记账**并可选告警 ✓ —— 与「零值 `Kind` 不得当作
// `KindStorage`」同型（设计 §1.2 ✓）。
func (k LogicalKey) IsZero() bool { return k.Owner == "" && k.Rel == "" }

// String 返回可读形式（日志/错误用；**不是**卷内键 ✗ —— 卷内键由各卷的 `Location.FSPath()`
// 等契约派生 ✓，本方法只供人类阅读 ✓）。
func (k LogicalKey) String() string {
	if k.IsZero() {
		return "<no-attribution>"
	}
	if k.Rel == "" {
		return k.Owner
	}
	return k.Owner + "/" + k.Rel
}
