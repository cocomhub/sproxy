// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// 本文件是 BaiduPCS-Go（Apache-2.0，https://github.com/qjfoidnh/BaiduPCS-Go）
// pcsutil/cachepool 的裁剪版本：仅保留 RawMallocByteSlice（netdisksign 签名生成所需），
// 移除 CachePool 池化与汇编实现（展示/并发无关，且汇编文件跨平台编译不便）。

// Package cachepool 提供字节切片分配工具（裁剪自 BaiduPCS-Go）。
package cachepool

// RawMallocByteSlice 分配一个新的 byte slice。
func RawMallocByteSlice(size int) []byte {
	return make([]byte, size)
}
