// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// write_file_progress_test.go 锁定「单次上传主路径（`WriteFile`，`POST /upload`）也装配服务端
// 写入阶段进度回调」。
//
// 背景（本片的核心证据）：进度回调原本只在 `WriteTarget` 装配，而单次上传走
// `Upload → WriteFile → writeFileSettle → writeTargetSettle`，**不经过 `WriteTarget`**
// ⇒ 卷层（加密/远端）经 ctx 上报的阶段进度全为 no-op，上层按 filename 轮询
// `/upload/status` 永远看不到阶段。本用例让外部卷 sink 在**写入期**经 ctx 上报阶段进度，
// 断言注册表出现**写入层口径**（`tnt.UserRel` → `user/<rel>`）的条目——去掉 `WriteFile` 的
// 注入即红。

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestService_WriteFile_InstallsUploadProgress 覆盖单次上传路径的阶段进度接线：外部卷 sink
// 在写入期经 ctx 上报 StageRemote ⇒ 注册表出现 `user/p.bin` 的进度（键为 rel 口径）。
func TestService_WriteFile_InstallsUploadProgress(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newExternalChunkEnv(t)
	svc := env.handlers(4)
	// 外部卷 sink 在写入期上报阶段进度（模拟远端卷真实上报，经写入 ctx 回调）。
	env.sink.onWrite = func(ctx context.Context) {
		syncpkg.ReportUploadProgress(ctx, syncpkg.StageRemote, 6, 12)
	}
	body := []byte("single-upload-body")
	if _, err := svc.WriteFile(context.Background(), WriteFileInput{
		Owner: "alice", RemotePath: "p.bin", ExplicitVol: "v-ext",
		ExpectedChecksum: sha256Hex(body), ClientSize: int64(len(body)),
	}, bytes.NewReader(body)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 注册表键必须是写入层口径的逻辑路径（tnt.UserRel：user/<rel>）。
	got, ok := svc.progressRegistry().get("user/p.bin")
	if !ok {
		t.Fatal("单次上传路径未装配进度回调：注册表无 user/p.bin 的阶段进度")
	}
	if got.Stage != string(syncpkg.StageRemote) || got.RemoteDone != 6 || got.RemoteTotal != 12 {
		t.Fatalf("阶段进度不符: %+v", got)
	}
}
