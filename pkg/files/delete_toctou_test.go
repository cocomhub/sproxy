// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// delete_toctou_test.go 是删除族 TOCTOU（time-of-check to time-of-use）窗口的钉住测试：
// checksum 校验与删除之间目标被并发替换时，删除必须作用于**被校验过的对象**，不得静默
// 删掉替换后的新文件（2026-09-17 安全加固，计划 2026-09-17-security-toctou.md 任务 1）。
//
// 手法：rename-to-quarantine——先原子重命名目标到独立中间路径（`.deleting.<nano>`），
// 校验 quarantine 内容匹配才删除；窗口内的路径替换只影响原 rel，不影响被校验/被删除的
// 对象。测试经 deleteBeforeRemoveHook（仅测试可见的注入点，默认 nil 零行为）模拟
// 「校验已通过、删除尚未执行」时并发写者替换原路径。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestService_Delete_TOCTOU_SwapBetweenCheckAndRemove 验证：校验通过后、删除执行前
// 原路径被替换为新内容时，删除的是**被校验过的旧对象**（original），替换后的新文件
// （swapped）必须原样保留，且无 quarantine 残留。
func TestService_Delete_TOCTOU_SwapBetweenCheckAndRemove(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态；seam 挂在实例（env.svc）上，
	// 不影响其它并行用例。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const original = "original-content"
	const swapped = "swapped-content"
	writeUserFile(t, env, "alice", "user/f.txt", original)

	// 注入 seam：校验已通过、删除尚未执行时，把原路径替换为新内容（模拟并发写者）。
	env.svc.deleteBeforeRemoveHook = func() {
		abs := filepath.Join(env.root, "alice", "user", "f.txt")
		if err := os.WriteFile(abs, []byte(swapped), 0o644); err != nil {
			t.Errorf("hook 替换文件失败: %v", err)
		}
	}
	t.Cleanup(func() { env.svc.deleteBeforeRemoveHook = nil })

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte(original))))

	if rr.Code != http.StatusOK {
		t.Fatalf("删除应 200（删的是被校验过的旧对象）, got %d: %s", rr.Code, rr.Body.String())
	}
	// 替换后的新文件必须保留：删除不得作用于它。
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != swapped {
		t.Fatalf("替换后的新文件应保留 %q, got %q", swapped, got)
	}
	// 无 quarantine 残留（删除路径已清理干净）。
	entries, err := os.ReadDir(filepath.Join(env.root, "alice", "user"))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "f.txt.deleting.") {
			t.Fatalf("不应有 quarantine 残留: %s", e.Name())
		}
	}
}

// TestService_Delete_TOCTOU_ChecksumMismatchRestores 验证：quarantine 内容与声明
// checksum 不匹配时，删除被拒绝（400）且文件恢复回原路径（不丢用户数据）。
func TestService_Delete_TOCTOU_ChecksumMismatchRestores(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const keep = "keep-me"
	writeUserFile(t, env, "alice", "user/f.txt", keep)

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte("other"))))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("checksum 不匹配应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != keep {
		t.Fatalf("拒绝分支应保留原文件 %q, got %q", keep, got)
	}
	entries, err := os.ReadDir(filepath.Join(env.root, "alice", "user"))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "f.txt.deleting.") {
			t.Fatalf("不匹配分支不应有 quarantine 残留: %s", e.Name())
		}
	}
}
