// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_restart_upload_test.go 覆盖**分块上传在 kill -9 后的恢复语义**（真实进程）。
//
// 实测（本用例）：分块会话的已收块**落盘**，重启后 `/upload/status` 仍报告已收块数 ⇒
// 客户端可以**续传剩余块**（无需从头重传），`complete` 后内容一致。
//
// 因此这里钉住三条：
//  1. 会话状态**跨重启存活**（received_count 不丢）；
//  2. 只用剩余块即可完成（证明前面已收的块没有丢/没有重复要求）；
//  3. 未完成的会话不会产生「半成品正式文件」（`/download` 404）。
package sproxy_test

import (
	"net/http"
	"testing"
	"time"
)

// TestE2E_Restart_ChunkedUploadResumesAfterKill9 分块上传在重启后**断点续传**。
func TestE2E_Restart_ChunkedUploadResumesAfterKill9(t *testing.T) {
	env := newRestartEnv(t, "")
	t.Cleanup(env.Close)
	env.boot(t)

	const chunkSize = int64(128 << 10)
	content := make([]byte, 512<<10)
	for i := range content {
		content[i] = byte(i % 251)
	}
	const uploadID = "up-kill9-session"
	modTime := time.Now()
	fileSum := sha256hex(content)
	total := int((int64(len(content)) + chunkSize - 1) / chunkSize)

	if r := doInit(t, env.baseURL, uploadID, "up.bin", content, chunkSize, modTime, fileSum); !r.Success {
		t.Fatalf("init 失败: %+v", r)
	}
	for i := 0; i < 2; i++ {
		uploadChunkE(t, env.baseURL, uploadID, i, chunkSize, content)
	}
	if st := doStatus(t, env.baseURL, uploadID); !st.Success || st.ReceivedCount != 2 {
		t.Fatalf("重启前应已收 2 块, got success=%v received=%d", st.Success, st.ReceivedCount)
	}

	// ① 未完成时不得产生正式文件。
	if status, _, body := downloadFile(t, env.baseURL, "up.bin"); status == http.StatusOK {
		t.Fatalf("未完成的上传不得留下正式文件（got 200, %d bytes）", len(body))
	}

	// 中断并重启（同存储根）。
	env.Kill9(t)
	env.boot(t)

	// ② 会话状态必须跨重启存活（否则客户端要整文件重传）。
	st := doStatus(t, env.baseURL, uploadID)
	if !st.Success {
		t.Fatalf("重启后会话丢失（success=false）—— 分块上传无法断点续传: %+v", st)
	}
	if st.ReceivedCount != 2 {
		t.Fatalf("重启后已收块数=%d, want 2（会话块落盘应存活）", st.ReceivedCount)
	}

	// ③ 只补剩余块即可完成（证明已收的块没有丢）。
	for i := 2; i < total; i++ {
		uploadChunkE(t, env.baseURL, uploadID, i, chunkSize, content)
	}
	if c := doComplete(t, env.baseURL, uploadID); !c.Success {
		t.Fatalf("complete 失败: %+v", c)
	}

	status, _, got := downloadFile(t, env.baseURL, "up.bin")
	if status != http.StatusOK {
		t.Fatalf("下载 status=%d", status)
	}
	if sha256hex(got) != fileSum {
		t.Fatalf("续传完成后内容不一致（len=%d want=%d）", len(got), len(content))
	}
}
