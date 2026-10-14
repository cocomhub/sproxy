// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"testing"
)

// delete_soft_external_test.go 钉住「外部卷软删必须 fail-closed」这条破坏性语义契约。
//
// 背景：软删是**显式**请求（`?soft=true` ⇒ 回收站、可恢复），而外部卷（加密/远端）没有回收站实现
// （trash 桶在本地租户根）。此前 `deleteExternalVolume` **无条件硬删** ⇒ 用户显式要求「可恢复删除」
// 却得到「不可恢复删除」= **静默降级 + 数据丢失**（本项目明令禁止的缺陷形态）。
//
// 变异：去掉 SoftDelete 的 fail-closed 分支 ⇒ 第一条用例红（ExternalDelete 被调用 = 数据已不可恢复）。

// TestDeleteFile_ExternalVolume_SoftDeleteFailClosed：显式软删 ⇒ 必须报错且**不触碰数据**。
func TestDeleteFile_ExternalVolume_SoftDeleteFailClosed(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{delOK: true}
	env := renameExternalEnv(t, ops)

	_, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.bin", ExplicitVol: "vault",
		SoftDelete: true, SkipChecksumVerify: true,
	})
	if err == nil {
		t.Fatal("外部卷软删必须 fail-closed（禁止静默降级为不可恢复硬删）")
	}
	// 决定性断言：被拒时**不得**调用 ExternalDelete——否则数据已经不可恢复，报错也晚了。
	if len(ops.deletes) != 0 {
		t.Fatalf("软删被拒时不得调用 ExternalDelete，got %d 次（%+v）", len(ops.deletes), ops.deletes)
	}
}

// TestDeleteFile_ExternalVolume_HardDeleteStillWorks：硬删（非 soft）不受影响（零回归）。
func TestDeleteFile_ExternalVolume_HardDeleteStillWorks(t *testing.T) {
	t.Parallel()
	ops := &fakeExternalOps{delOK: true}
	env := renameExternalEnv(t, ops)

	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.bin", ExplicitVol: "vault",
		SkipChecksumVerify: true,
	}); err != nil {
		t.Fatalf("外部卷硬删失败: %v", err)
	}
	if len(ops.deletes) != 1 {
		t.Fatalf("硬删应调用 ExternalDelete 恰好 1 次，got %d", len(ops.deletes))
	}
}
