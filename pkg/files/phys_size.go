// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import "github.com/cocomhub/sproxy/pkg/storage"

// phys_size.go 是**容量记账口径**的唯一事实源（2026-10-14 加密卷容量口径收口）。
//
// 口径：账本（owner Scope + 卷容量池）按**物理占用**记账。at-rest 加密卷（`extra.encrypt`）的物理
// 字节 > 逻辑字节（每 64KiB 块多 4B 长度前缀 + nonce + GCM tag，另有文件头），按逻辑长度记账会
// 低估：卷池靠周期对账（磁盘扫描）收敛，而 **owner 配额侧永不收敛** ⇒ 短时可超限。
//
// **不变式：写入/预留/结算/释放四处必须同口径**——只改一侧会造成配额漂移（少释放=配额虚低⇒超限
// 写入通过；多释放=幽灵占用⇒假 507）。故所有「按文件长度记账」的点一律经本文件的 helper 折算。
//
// 口径判定必须用**该文件所在卷的 root**（加密是**按卷**属性，多卷装配下 owner 默认租户的 root 可能
// 不是目标卷 ⇒ 取错口径）。

// RootPhysSizer 返回该根的记账口径换算（加密卷 ⇒ 物理占用；否则 nil = 恒等）。
func RootPhysSizer(root *storage.Root) func(int64) int64 {
	if root == nil || !root.IsEncrypted() {
		return nil
	}
	return storage.EncryptedPhysicalSize
}

// TenantPhysSizer 返回该租户根的记账口径换算（nil = 恒等）。装配层（pkg/server 多卷路由）与域内
// 单卷路由必须用**同一判据**决定口径，否则同一份数据在不同路由下记账不一致。
func TenantPhysSizer(tnt *storage.Tenant) func(int64) int64 {
	if tnt == nil {
		return nil
	}
	return RootPhysSizer(tnt.Root())
}

// physicalCharge 按 root 的口径把逻辑字节折算为记账字节（nil root / 非加密 / 非正值 ⇒ 原样返回）。
func physicalCharge(root *storage.Root, size int64) int64 {
	if size <= 0 {
		return size
	}
	if f := RootPhysSizer(root); f != nil {
		return f(size)
	}
	return size
}
