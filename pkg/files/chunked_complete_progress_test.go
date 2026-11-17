// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"net/http"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 2026-11-17 实测缺口（生产可用性·可观测性）：
//
//	`POST /upload/complete` 对外部卷（baidu）的**转存**（`completeExternalTransfer` →
//	`attemptExternalTransfer` → `sink.WriteFile`）是**长尾阶段**（实测 5.9GB 耗时 **16–20 分钟**），
//	但客户端进度页在这段时间恒显示「分块上传 100%」+「**疑似停滞**」✗，服务端也无任何阶段日志 ✗。
//
// 根因：外部卷写面 `externalUploadSink.WriteFile` **直连卷 FS**（`pkg/server/external_upload.go:66`），
// 绕过了 `pkg/files/write_ops.go` 里唯一的两处 `withUploadProgress` 注入点 ⇒ 进度 ctx 里
// **从未装配回调** ⇒ 卷层 `syncpkg.ReportUploadProgress`（`secretdata` 加密 / `baidupcs` 远端）
// **全为 no-op** ⇒ 注册表永远只有客户端分块进度。
//
// 修复：`completeExternalTransfer` 在调用写面前把进度回调注入 ctx（键口径与读侧 `progressRel`
// 一致）。本用例即锁定该行为。
//
// 变异：去掉 `completeExternalTransfer` 里的 `withUploadProgress` ⇒ 本用例红
// （`onWrite` 拿不到回调，注册表读不到 remote 阶段）。

// TestChunkedComplete_ExternalTransferPublishesStageProgress 钉住「转存期间卷层阶段上报可见」：
// ① 写面收到的 ctx **必须**带进度回调；② 回调上报必须落进**读侧同一键**（`progressRel`）。
func TestChunkedComplete_ExternalTransferPublishesStageProgress(t *testing.T) {
	t.Parallel()
	env := newExternalChunkEnv(t)
	h := env.handlers(4)

	const (
		uploadID = "ext-progress-1"
		filename = "jav/dir/长尾文件.bin"
	)
	content := []byte("0123456789abcdef") // 16B ⇒ 4 块（4,4,4,4）

	var (
		mu            sync.Mutex
		sawCallback   bool
		stageAtWrite  string
		keyAtWrite    string
		encryptAtMins int64
	)
	// 模拟「卷层在写入期经 ctx 上报阶段进度」：这是生产里 secretdata/baidupcs 的真实行为，
	// 只要 ctx 里装配了回调就会落进注册表。
	env.sink.onWrite = func(ctx context.Context) {
		fn, ok := syncpkg.UploadProgressFrom(ctx)
		mu.Lock()
		sawCallback = ok
		mu.Unlock()
		if !ok {
			return
		}
		// 加密阶段（明文口径）+ 远端阶段，与生产上报点同形。
		fn(syncpkg.StageEncrypt, 8, 16)
		fn(syncpkg.StageRemote, 16, 16)

		// 读侧：转存**进行中**按用户可见路径查询（与客户端 `/upload/status?filename=` 同路径）。
		rel := h.progressRel("alice", filename)
		if got, ok := h.progressRegistry().get(rel); ok {
			mu.Lock()
			stageAtWrite = got.Stage
			keyAtWrite = rel
			encryptAtMins = got.EncryptTotal
			mu.Unlock()
		}
	}

	out := env.uploadToExternal(t, h, uploadID, filename, content, 4)
	if out.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", out.Code, out.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawCallback {
		t.Fatal("外部卷转存的写面 ctx 未装配进度回调 ⇒ 卷层 加密/远端 阶段上报全为 no-op" +
			"（生产症状：客户端进度页在长尾转存期间恒显示「疑似停滞」）")
	}
	if stageAtWrite != string(syncpkg.StageRemote) {
		t.Fatalf("转存期间读侧应按**读侧同一键**看到 remote 阶段，实际 stage=%q key=%q",
			stageAtWrite, keyAtWrite)
	}
	if encryptAtMins != 16 {
		t.Fatalf("加密阶段总量应可读（明文口径），实际 %d", encryptAtMins)
	}
	if keyAtWrite != "user/"+filename {
		t.Fatalf("进度键应为 user/<filename>，实际 %q", keyAtWrite)
	}
}
