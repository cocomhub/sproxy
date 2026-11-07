// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// recover_ordering_gate_test.go 是「**恢复必须先完成 map 写入、之后才启动 goroutine**」门禁（R33）。
//
// 触发背景（独立审核 P1，2026-10-18）：`recoverTasksFromDir` 曾在循环里
// `m.tasks[task.ID] = &task`（**不持锁**）并对 `downloading` 任务立即
// `go m.executeDownload(...)` ⇒ 循环内的无锁写与运行中的 goroutine（持锁读写同一 map）**真并发**
// ⇒ `-race` 报 data race，极端时序 `fatal error: concurrent map read and map write`（启动阶段崩溃）。
//
// 为何需要门禁：该缺陷的 `-race` 检出是**概率性**的（交错窗口极短；同一用例实测一次红一次绿 ✗）
// ⇒ 不能只靠运行时用例。⇒ 锁定**结构顺序**：恢复路径必须先调用
// `insertRecoveredTasksLocked`（持锁批量插入），**之后**才调用 `activateRecoveredTasks`
// （启动下载 / 孤儿 pending）。顺序颠倒 ⇒ 本门禁必红。
//
// 判据（结构性、可判定）：`pkg/cloud/manager_persist.go` 中 `recoverTasksFromDir` 函数体内，
// `insertRecoveredTasksLocked` 的**首次出现位置**必须早于 `activateRecoveredTasks`。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecoverInsertsTasksBeforeActivating 恢复路径必须「先插完再启动」。
func TestRecoverInsertsTasksBeforeActivating(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	p := filepath.Join(root, "pkg", "cloud", "manager_persist.go")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	src := string(b)
	start := strings.Index(src, "func (m *CloudDownloadManager) recoverTasksFromDir(")
	if start < 0 {
		t.Fatal("未找到 recoverTasksFromDir（门禁失效：函数已改名/移动）")
	}
	// 函数体到下一个顶层 func 为止。
	rest := src[start:]
	if i := strings.Index(rest[1:], "\nfunc "); i >= 0 {
		rest = rest[:i+1]
	}
	ins := strings.Index(rest, "insertRecoveredTasksLocked(")
	act := strings.Index(rest, "activateRecoveredTasks(")
	if ins < 0 || act < 0 {
		t.Fatalf("恢复路径必须同时调用 insertRecoveredTasksLocked 与 activateRecoveredTasks"+
			"（insert=%d activate=%d）—— 该拆分正是「map 写入」与「启动 goroutine」的时间分离", ins, act)
	}
	if ins > act {
		t.Error("recoverTasksFromDir 里 activateRecoveredTasks（启动下载 goroutine）出现在 " +
			"insertRecoveredTasksLocked（持锁批量插入）**之前** ⇒ 循环内的 map 写入会与运行中的 " +
			"goroutine 并发（data race / 启动阶段 fatal error）。必须：先收集 → 持锁插入 → 再启动")
	}
	// 循环体内不得直接写 m.tasks（必须只在 insertRecoveredTasksLocked 里写）。
	if strings.Contains(rest, "m.tasks[p.task.ID] = p.task") {
		t.Error("recoverTasksFromDir 内出现 m.tasks 写入 —— map 写入必须只在 insertRecoveredTasksLocked" +
			"（持锁）里发生")
	}
}
