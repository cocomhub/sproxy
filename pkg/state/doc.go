// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package state 提供统一状态存储抽象（StateStore）与配套能力接口。
//
// 背景（roadmap 11.12，设计 2026-09-24-statestore.md）：sproxy 各状态
// （凭据/checksum/dedup/share/index/audit）此前均为各自为政的本地 JSON 落盘，
// 每份都重复「tmp+rename 原子写 + saveMu 串行化」样板且无 CAS 原语。本包定义
// 跨实现一致的 StateStore 接口（本地 JSON 为默认实现，mongo/raft 为插件位），
// 装配层经注册表分派，把「状态落哪里」从具体 Store 解耦。
//
// 安全边界：
//   - key 为三段式 `<owner>/<type>/<name>`，每段必须过 storage.ValidSegmentName
//     语义的段名校验（拒绝 .. / 绝对路径 / 空字节 / Windows 非法字符）；name 段
//     内部允许 `/`（如 checksum/<owner>/<rel> 的 rel 本身就是路径）；
//   - 非法 key 一律 fail-closed 返回错误，绝不静默改写或截断；
//   - 本地实现落 `<root>/state/<owner>/<type>/<name>.json`（name 段内部 `/`
//     转目录层级），原子写（tmp + rename）保证并发安全；
//   - 领域包不反向依赖装配层：适配（credential/checksum/dedup/share/index 走
//     StateStore）发生在装配层（pkg/server），领域包保持既有接口不动。
package state
