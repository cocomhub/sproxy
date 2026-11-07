// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"sync/atomic"
)

// encRootChunkOverheadCached 缓存每块开销（首次调用由 cipher 实例派生）。
var encRootChunkOverheadCached atomic.Pointer[int]

// encrypted_size.go 给出 **at-rest 加密卷的物理占用精确公式**（2026-10-14 容量口径收口）。
//
// 动机：加密卷的物理字节 > 逻辑字节（每块 4B 长度前缀 + nonce + GCM tag），而容量账本此前按
// **逻辑长度**记账 ⇒ 账本低估物理占用（评审记档「加密卷容量按逻辑长度记账 ~0.1% 偏差」），
// 卷池虽由周期对账收敛，owner 配额侧则永不收敛 ⇒ 短时可超限。本文件把公式与写入格式**同源**
// 放在一起（常量/派生都取自写入器实际使用的密码学参数），供写路径按物理口径记账。
//
// 公式（与 `encRootWriter` 的落盘格式逐字节对应）：
//
//		physical(n) = headerLen + n + chunkOverhead * ceil(n / chunk)
//
//	  - `headerLen` = `len(encRootMagic) + 4`（魔数 + 4B 块大小）；
//	  - `chunk`     = 新文件的默认块大小（`encRootDefaultChunk`）；
//	  - `chunkOverhead` = 4（长度前缀）+ `gcm.NonceSize()` + `gcm.Overhead()`（GCM tag）
//	    ——**由 cipher 实例派生**而非硬编码，格式若变则公式随之而变（防「常量与实现漂移」）。
//
// 边界：`n <= 0` ⇒ 仅文件头（空文件仍是 `headerLen` 字节——写入器建文件即写头）。
func EncryptedPhysicalSize(logical int64) int64 {
	if logical < 0 {
		logical = 0
	}
	chunks := (logical + encRootDefaultChunk - 1) / encRootDefaultChunk
	return int64(encRootHeaderLen) + logical + int64(encRootChunkOverhead())*chunks
}

// encRootHeaderLen 是加密文件的固定头长度（魔数 + 4B 块大小）。
// 注意是 var（`encRootMagic` 是切片，`len()` 非编译期常量）——初始化后只读。
var encRootHeaderLen = len(encRootMagic) + 4

// encRootDefaultChunk 是新建加密文件的默认块大小（与 `newEncRootWriter` 的回落值同源）。
const encRootDefaultChunk = 64 << 10

// encRootChunkOverhead 返回每个密文块的固定开销（4B 长度前缀 + nonce + GCM tag）。
// 由 cipher 实例派生（非硬编码）：AES-256-GCM 下为 4 + 12 + 16 = 32B。
func encRootChunkOverhead() int {
	if v := encRootChunkOverheadCached.Load(); v != nil {
		return *v
	}
	overhead := 4 + 12 + 16 // 理论不可达时的保守回落（AES-GCM 标准参数）
	if block, err := aes.NewCipher(make([]byte, 32)); err == nil {
		if gcm, gerr := cipher.NewGCM(block); gerr == nil {
			overhead = 4 + gcm.NonceSize() + gcm.Overhead()
		}
	}
	encRootChunkOverheadCached.Store(&overhead)
	return overhead
}
