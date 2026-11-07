// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package integrity 提供云端下载后按类型语义校验的 Checker 注册表（零外部依赖、
// shardseal 注册表模式）。任务 2 实现 image/tar 校验器，经 Register 装配进默认注册表
// 或本地 Registry；ext/video 等带外部依赖的校验器走独立 module。
//
// 语义（对齐 specs/2026-10-06-cloud-download-integrity-design.md §4）：
//   - Checker.Matches 按文件名扩展名判定归属；Lookup 分发命中第一个注册校验器。
//   - Lookup 返回 nil = 无对应校验器 → 调用方按「仅字节级校验」处理（未知类型不误报
//     damaged——Review Focus 1）。
package integrity

import "errors"

// ErrDuplicateKind 是 Register 重复注册同 Kind 的哨兵错误（fail-fast panic 携带）。
var ErrDuplicateKind = errors.New("integrity: 重复注册 Kind")
