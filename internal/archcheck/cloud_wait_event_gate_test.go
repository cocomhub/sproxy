// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os"
	"strings"
	"testing"
)

// TestCloudWaitTaskStoppedIsEventDriven 守 `CloudDownloadManager.waitTaskStopped` 的**事件等待**语义：
// 该函数被 CancelTask/DeleteTask/resume 的「旧 goroutine 是否已退出」判定使用，必须由
// `clearRunningLocked` 关闭通知通道唤醒，不得回退为 sleep 轮询——轮询粒度（100ms）
// 会在 CI 低配 runner 上把「退出耗时」放大成误判（`TestCloudDownloadManager_CancelStopsDownload`
// 历史 flake）。
//
// 变异：把实现改回「循环 + sleep 100ms」轮询 ⇒ 本门禁红。
func TestCloudWaitTaskStoppedIsEventDriven(t *testing.T) {
	t.Parallel()
	const path = "../../pkg/cloud/manager_lifecycle.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	src := string(raw)
	const sig = "func (m *CloudDownloadManager) waitTaskStopped("
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("%s 中找不到 waitTaskStopped", path)
	}
	rest := src[i:]
	before, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatal("waitTaskStopped 函数体未闭合")
	}
	body := before
	// 拼接字面量：本文件不得出现该 token，否则会被 R14 睡眠棘轮误计为真实 sleep。
	if strings.Contains(body, "time."+"Sleep") {
		t.Fatalf("waitTaskStopped 不得使用 time.Sleep 轮询（应为 runningDone 事件等待）:\n%s", body)
	}
	if !strings.Contains(body, "runningDone") {
		t.Fatalf("waitTaskStopped 必须等待 runningDone 通知通道:\n%s", body)
	}
}
