// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package slogutil 提供 `log/slog` 的**公共基础设施**（不是领域抽象）：
// 目前只有「nil logger 归一」这一项——把 nil 归一为 `slog.Default()`，
// 使构造函数可以安全接受可选 logger。
//
// 为什么放 `internal/` 而不是 `pkg/`：它是**实现细节的共享**，不是可复用的领域能力，
// 放进 `pkg/` 会让它成为对外承诺的公共 API（与 `internal/archcheck` 同理）。
// 跨 module 仍可用：`internal/` 的可见性按导入路径前缀判定，
// `github.com/cocomhub/sproxy/cmd/*` 等子 module 同样在前缀内。
//
// 为什么需要它（实测）：该函数此前在 5 个包各有一份**同语义私有副本**
// （pkg/checksum、pkg/server、pkg/storage/capacity、pkg/syncmgr、pkg/cloud），
// 每份都带着「随包搬迁的私有依赖」注释——那是「包之间不能互相导入」造成的重复，
// 而不是真的需要各自一份。收敛后由 `internal/archcheck` 的
// TestNoDefaultLoggerDuplication 守卫，禁止再出现本地副本。
//
// 注意与 `pkg/tunnel/hub/ext/kad` 的同名函数区分：那个返回 **Discard** logger
// （语义不同，且位于独立 module）——不作为本函数的消费者，也不受该守卫约束。
package slogutil

import "log/slog"

// Default 返回一个有效的 *slog.Logger：l 为 nil 时返回 slog.Default()，否则原样返回。
func Default(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
