// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

import "context"

// ChecksumProvider 是 FS 的**可选能力**：只取「整文件 checksum + 大小」，
// **不要求**实现者加载冷结构（分块 / blocklets / parity）。
//
// 为什么需要（P5「字段级别路由」，2026-11-13）：
// 实测 `secretdata` 单个大文件的 `Meta` 常驻 **78 MB**、解密+解析 ≈ **584 ms**，
// 而多处调用方**只用 `TotalSHA256`（+ Size）** —— 典型如外部卷 `ExternalStat`
// （**热路径**，每次 stat 都会走到）。让它们走这条轻量路径，可**完全避免冷结构加载**。
//
// 契约：
//   - 有 checksum 但未知时返回错误（不要用空串伪装成功）；
//   - **未实现者 ⇒ 调用方回落 `Provider.FileMeta`**（零回归）。
type ChecksumProvider interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义
	// ChecksumAndSize 返回整文件内容 checksum（通常 SHA-256 hex）与字节大小。
	ChecksumAndSize(ctx context.Context, rel string) (checksum string, size int64, err error)
}

// AssertChecksumProvider 查询 FS 是否支持**轻量 checksum 路径**（未实现 ⇒ ok=false）。
func AssertChecksumProvider(v any) (ChecksumProvider, bool) {
	p, ok := v.(ChecksumProvider)
	return p, ok
}
