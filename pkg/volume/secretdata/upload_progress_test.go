// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// upload_progress_test.go 锁定二期 T2（2026-11-10）：**加密阶段必须上报进度**。
//
// 语义：加密器逐块交付（`interleavedChunkSink`）⇒ 每交付一块上报一次
// `ReportUploadProgress(ctx, StageEncrypt, 已加密字节, 总字节)`。
// 进度页据此显示「加密 42%（1.9/4.6GB）」（sdserver 侧经 /upload/status 轮询取得）。
//
// 变异验证：删掉 `interleavedChunkSink` 里的 `ReportUploadProgress` 调用 ⇒ 本用例红。

import (
	"bytes"
	"context"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestWriteFileStream_ReportsEncryptProgress 加密阶段上报契约：
// ① 至少有 1 次上报；② stage 全是 encrypt；③ total == size；④ done 单调递增且最终 == size。
func TestWriteFileStream_ReportsEncryptProgress(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var donts []int64
	var allEncrypt, called bool
	var lastTotal int64

	ctx := syncpkg.WithUploadProgress(context.Background(),
		func(stage syncpkg.UploadStage, done, total int64) {
			mu.Lock()
			defer mu.Unlock()
			called = true
			lastTotal = total
			donts = append(donts, done)
			if stage != syncpkg.StageEncrypt {
				allEncrypt = false
			}
		})
	// 先置为 true：只要出现一次非 encrypt 就落回 false（便于断言"全是 encrypt"）。
	allEncrypt = true

	sfs := newFS(t)
	payload := bytes.Repeat([]byte("enc-progress-"), 8*1024) // 96KiB（多块）
	const mtime = int64(1700000000000000000)
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/e.bin",
		bytes.NewReader(payload), int64(len(payload)), mtime); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Fatal("加密阶段必须上报进度（StageEncrypt）；未收到任何上报 ⇒ 进度页无法显示加密进度")
	}
	if !allEncrypt {
		t.Fatal("上报的 stage 必须全是 encrypt")
	}
	if lastTotal != int64(len(payload)) {
		t.Fatalf("total 应为明文总字节 %d，got %d", len(payload), lastTotal)
	}
	if len(donts) == 0 || donts[len(donts)-1] != int64(len(payload)) {
		t.Fatalf("最终 done 应为总字节 %d，got %v", len(payload), donts)
	}
	for i := 1; i < len(donts); i++ {
		if donts[i] < donts[i-1] {
			t.Fatalf("done 必须单调递增，got %v", donts)
		}
	}
}

// TestWriteFileStream_NoProgressHookIsNoop 未注入回调时写入必须照常成功（零回归）。
func TestWriteFileStream_NoProgressHookIsNoop(t *testing.T) {
	t.Parallel()
	sfs := newFS(t)
	payload := bytes.Repeat([]byte("no-hook-"), 4*1024)
	if err := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "d/n.bin",
		bytes.NewReader(payload), int64(len(payload)), 1700000000000000000); err != nil {
		t.Fatalf("未注入进度回调时写入必须成功: %v", err)
	}
}
