// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloudStopSignalAfterRelease（R48）守「waitTaskStopped 返回 ⇒ 配额已归零」不变量：
// `cleanupRunning` 必须先清 running、再释放配额（`releaseAbandonedTaskScope`）、**最后**才
// 关闭 `runningDone` 通知通道（`signalStoppedLocked`）。
//
// 反例（CI 偶发红 `CancelDuringWrite_QuotaZero` 的 `Usage()=400 want 0`）：通知与清 running
// 放在同一函数 ⇒ 等待方在释放完成前被唤醒并读到残留占用。
//
// 变异：把 `signalStoppedLocked` 移到 `releaseAbandonedTaskScope` 之前 ⇒ 本门禁红。
func TestCloudStopSignalAfterRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(moduleRoot(t), "pkg", "cloud", "manager_task.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	src := string(raw)
	// 只看 cleanupRunning 闭包体（defer cleanupRunning() 之前）。
	start := strings.Index(src, "cleanupRunning := func()")
	end := strings.Index(src, "defer cleanupRunning()")
	if start < 0 || end <= start {
		t.Fatal("未找到 cleanupRunning 闭包（扫描面被改窄）")
	}
	body := src[start:end]
	for _, token := range []string{"m.clearRunningLocked(", "m.releaseAbandonedTaskScope(", "m.signalStoppedLocked("} {
		if !strings.Contains(body, token) {
			t.Fatalf("cleanupRunning 缺少 %s（释放/通知链路不完整）:\n%s", token, body)
		}
	}
	clearAt := strings.Index(body, "m.clearRunningLocked(")
	releaseAt := strings.Index(body, "m.releaseAbandonedTaskScope(")
	signalAt := strings.Index(body, "m.signalStoppedLocked(")
	if clearAt >= releaseAt || releaseAt >= signalAt {
		t.Fatalf("cleanupRunning 顺序必须是 clear → release → signal（got clear=%d release=%d signal=%d）:\n%s",
			clearAt, releaseAt, signalAt, body)
	}
}
