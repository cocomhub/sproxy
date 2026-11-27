// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build race

package testutil

// RaceEnabled 表示当前二进制由 `-race` 构建（竞态检测器开启）。
//
// 为什么需要它（2026-11-27）：`-race` 会把执行拖慢 5–10 倍，再叠加**全包 `t.Parallel`**
// 的 CPU 抢占（本仓 `pkg/cloud` 单包几十个并行下载用例），固定等待窗口（5s/10s/30s）
// 会把"只是慢"误报成"goroutine 泄漏 / 逻辑超时"。实测：单跑 4.1s 的用例在全包 `-race`
// 下 28s+ 才完成。
//
// 用它来**放大等待窗口**而不是直接调大所有常量：非 race 构建（本地 `go test`、CI 的
// 非 race job）保持原有严格窗口，不放宽真实缺陷的检出能力。
const RaceEnabled = true
