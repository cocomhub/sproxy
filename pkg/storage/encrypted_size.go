// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import "github.com/cocomhub/sproxy/pkg/cipherstream"

// encrypted_size.go 给出 **at-rest 加密卷的物理占用精确公式**（2026-10-14 容量口径收口）。
//
// 动机：加密卷的物理字节 > 逻辑字节（每块 4B 长度前缀 + nonce + GCM tag），而容量账本此前按
// **逻辑长度**记账 ⇒ 账本低估物理占用（评审记档「加密卷容量按逻辑长度记账 ~0.1% 偏差」），
// 卷池虽由周期对账收敛，owner 配额侧则永不收敛 ⇒ 短时可超限。本文件把公式与写入格式**同源**
// 放在一起——常量与开销**全部取自 pkg/cipherstream**（格式的唯一实现），常量无从漂移。
//
// 公式（与 cipherstream 的落盘格式逐字节对应）：
//
//		physical(n) = headerLen + n + chunkOverhead * ceil(n / chunk)
//
//	  - `headerLen`     = `cipherstream.HeaderLen(encRootMagic)`（魔数 + 4B 块大小）；
//	  - `chunk`         = `cipherstream.DefaultChunkSize`（新建文件的默认块大小）；
//	  - `chunkOverhead` = `cipherstream.ChunkOverhead()`（4B 长度前缀 + nonce + GCM tag，
//	    由真实 cipher 实例派生而非硬编码）。
//
// 边界：`n <= 0` ⇒ 仅文件头（空文件仍是 `headerLen` 字节——写入器建文件即写头）。
func EncryptedPhysicalSize(logical int64) int64 {
	if logical < 0 {
		logical = 0
	}
	chunk := int64(cipherstream.DefaultChunkSize)
	chunks := (logical + chunk - 1) / chunk
	return int64(cipherstream.HeaderLen(encRootMagic)) + logical + int64(cipherstream.ChunkOverhead())*chunks
}
