// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"time"
)

// waitTB 是 WaitFor 需要的最小测试接口。
//
// 为什么不直接收 testing.TB：testing.TB 含私有方法（private()），**包外无法实现替身**，
// 于是「超时确实调用 Fatalf」这条分支就无法自测（只能靠人眼）。用一个只含所需方法的窄接口，
// 既能收 *testing.T，也能在测试里放一个假实现。
type waitTB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// waitPollInterval 是 WaitFor 的轮询间隔：2ms 远小于任何真实异步操作的完成时间，
// 又不会把 CI 打满（对比固定 sleep：空闲机上白等、繁忙机上可能不够长）。
const waitPollInterval = 2 * time.Millisecond

// WaitFor 轮询等待 cond 返回 true；超时则让测试失败（msg 会附在失败信息里）。
//
// 为什么要有它：测试里的 `time.Sleep(10 * time.Millisecond)` 固定等待有两重问题——
// ① 繁忙 CI 上可能**不够长**（flake 温床，本仓已多次踩到）；② 空闲机器上又白等，累积成
// 分钟级拖慢。条件轮询既有确定性的失败信号，又能在条件一满足就立刻返回。
//
// 语义要点：
//   - cond 在**调用方 goroutine** 里执行（不另起 goroutine），因此可以安全地调用
//     t.Fatalf/t.Errorf 并读写测试局部变量；
//   - cond **首次立即执行**（已经满足时零等待）；
//   - 超时消息必带 timeout 与调用方说明——flake 报告要能自解释，不能只说「超时了」。
//
// 诊断可以传**动态消息**：msg 的唯一元素若为 `func() string`，则在超时那一刻求值——
// 手写循环能写「最后观测到的状态」，换成本助手不应丢失这个能力：
//
//	var last string
//	testutil.WaitFor(t, 5*time.Second, func() bool {
//		cur, ok := mgr.SnapshotTask(id, "")
//		if ok { last = cur.Status }
//		return ok && cur.Status == "completed"
//	}, func() string { return "任务未完成，最后状态: " + last })
//
// 用法：
//
//	testutil.WaitFor(t, time.Second, func() bool {
//		cur, ok := mgr.SnapshotTask(id, "")
//		return ok && cur.Status == "downloading"
//	}, "任务应进入 downloading")
func WaitFor(tb waitTB, timeout time.Duration, cond func() bool, msg ...any) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			if len(msg) > 0 {
				tb.Fatalf("WaitFor 超时（%s）：%s", timeout, formatWaitMsg(msg))
			} else {
				tb.Fatalf("WaitFor 超时（%s）", timeout)
			}
			// 显式 return：testing.T.Fatalf 会 runtime.Goexit（不会返回），但窄接口的替身
			// 不会——不返回就会在假实现下无限循环（写本助手时实测踩到，由 wait_test.go 钉住）。
			return
		}
		time.Sleep(waitPollInterval)
	}
}

// formatWaitMsg 渲染超时诊断：单个 `func() string` 参数在**超时时刻**求值（可携带最后一次观测），
// 其余情况按 fmt.Sprint 拼接。
func formatWaitMsg(msg []any) string {
	if len(msg) == 1 {
		if f, ok := msg[0].(func() string); ok {
			return f()
		}
	}
	return fmt.Sprint(msg...)
}
