// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package units 是单位抽象的统一宿主（2026-10-03 用户裁决）：
// 字节大小、时长、速率等后续单位类型统一以**子包**置于本包下，按单位类型划分——
//
//	units/sizex  → 字节大小（ByteSize int64 + ParseSize，人类可读 "1GiB"/纯数字）
//	units/...    → 时长 / 速率等后续单位（预留，未实现，按需新增）
//
// 父包仅承载「单位类抽象统一放此」的方向说明与文档，不提前造接口（YAGNI）：
// 每个单位子包自包含（类型 + 解析 + 序列化 + 测试），不依赖其它子包。
//
// 强制约定（AGENTS.md / sproxy/CLAUDE.md 工程原则）：字节大小配置一律用
// `pkg/units/sizex.ByteSize`（"1GiB" 人类可读），**禁止在 config 结构裸写 int64 字节字段**；
// 新增单位类字段须遵循「units/<子包> 自包含」模式。
package units
