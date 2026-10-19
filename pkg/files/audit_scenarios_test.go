// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// audit_scenarios_test.go 锁定设计 docs/designs/2026-10-19-audit-service.md §9.1 的
// 业务功能「审计场景」（用户要求⑤：生产可用后必须补各业务场景用例，锁定防回归）：
// 每个写面操作都必须在**域层**留下可断言的 action/object/result/detail 审计记录。
//
// 为何在域层断言：审计在 `Service` 层实现（write_ops/tags…）⇒ 同源覆盖 HTTP/DAV/S3/CLI
// 全部 face；若有人把审计挪回某个 HTTP handler，本组用例仍绿而 DAV/S3 面会静默丢失 ——
// 因此配合门禁 R35-a（调用点棘轮）共同锁定。

import (
	"context"
	"net/http"
	"testing"
)

// assertAuditAction 断言存在指定 action 的**成功**审计行（object 由实现决定：
// copy/move/rename 等可能在源侧或目标侧记，故按 action 断言，不锁 object）。
func assertAuditAction(t *testing.T, env *dirsEnv, action string) auditRow {
	t.Helper()
	for _, row := range env.audits {
		if row.action == action && row.result == auditResultSuccess {
			if row.detail == "" && action != "delete" {
				t.Errorf("%s 审计应带 detail 上下文, got 空", action)
			}
			return row
		}
	}
	t.Fatalf("应记录 %s 成功审计, 实得 %+v", action, env.audits)
	return auditRow{}
}

// TestAuditScenario_FileLifecycle 覆盖文件全生命周期：
// 新上传 → 覆盖 → 改名 → 复制 → 移动 → 硬删（每步都必须留痕）。
func TestAuditScenario_FileLifecycle(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("audit-lifecycle")
	if rr := env.upload(t, "alice", "a.txt", body, sha256Hex(body), 0); rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertAuditAction(t, env, "upload")

	// 覆盖写被拒（versioning 关闭 + checksum 不匹配）⇒ **必须留痕为 denied**（§9.1「含 denied」）：
	// 静默拒绝是审计最危险的失败形态（用户以为改成功了）。
	body2 := []byte("audit-lifecycle-2")
	if rr := env.upload(t, "alice", "a.txt", body2, sha256Hex(body2), 0); rr.Code == http.StatusOK {
		t.Fatalf("checksum 不匹配的覆盖不应 200（versioning 关闭时拒绝覆盖）")
	}
	denied := false
	for _, row := range env.audits {
		if row.action == "overwrite" && row.object == "a.txt" && row.result == auditResultDenied {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("被拒覆盖必须记 overwrite/denied, got %+v", env.audits)
	}

	ctx := context.Background()
	if _, err := env.svc.RenameFile(ctx, RenameFileInput{Owner: "alice", From: "a.txt", To: "b.txt", ExpectedChecksum: sha256Hex(body)}); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	assertAuditAction(t, env, "rename")

	if _, err := env.svc.CopyFile(ctx, CopyFileInput{Owner: "alice", From: "b.txt", To: "c.txt"}); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	assertAuditAction(t, env, "copy")

	// 移动：**域实现 = 复制 + 删除**（跨目录移动不复用同卷 rename），审计如实反映两步 ——
	// 断言「目标已复制 + 源已删除」，而不是伪造一个 move 动作（如实优先于好看）。
	if _, err := env.svc.MoveFile(ctx, MoveFileInput{Owner: "alice", From: "c.txt", To: "d.txt"}); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	var movedCopy, movedDelete bool
	for _, row := range env.audits {
		if row.action == "copy" && row.object == "c.txt" && row.detail == "to=d.txt volume=" {
			movedCopy = true
		}
		if row.action == "delete" && row.object == "c.txt" && row.result == auditResultSuccess {
			movedDelete = true
		}
	}
	if !movedCopy || !movedDelete {
		t.Fatalf("移动应留「复制到 d.txt + 删除 c.txt」两条审计, got copy=%v delete=%v: %+v",
			movedCopy, movedDelete, env.audits)
	}

	if _, err := env.svc.DeleteFile(ctx, DeleteFileInput{Owner: "alice", RemotePath: "d.txt", ExpectedChecksum: sha256Hex(body)}); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	assertAuditAction(t, env, "delete")
}

// TestAuditScenario_DirOps 目录创建/删除必须留痕（此前只在 HTTP 层记 ⇒ DAV/S3 面漏记）。
func TestAuditScenario_DirOps(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	ctx := context.Background()
	if _, err := env.svc.MakeDir(ctx, "alice", "auditdir", ""); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	assertAuditAction(t, env, "mkdir")

	if _, err := env.svc.RemoveDir(ctx, "alice", "auditdir", "", true); err != nil {
		t.Fatalf("rmdir 失败: %v", err)
	}
	assertAuditAction(t, env, "rmdir")
}

// TestAuditScenario_Tags 标签更新（元数据变更）必须留痕。
func TestAuditScenario_Tags(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("audit-tags")
	if rr := env.upload(t, "alice", "h.txt", body, sha256Hex(body), 0); rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := env.serve(env.svc.Tags, "alice", "POST", "/api/tags?filename=h.txt&tags=one,two"); rr.Code != http.StatusOK {
		t.Fatalf("打标应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	assertAuditAction(t, env, "tags_update")
}

// TestAuditScenario_FailedOpNotRecordedAsSuccess 失败的写操作**不得**被记为成功
// （审计的可信度底线：宁可少记，不可错记）。
func TestAuditScenario_FailedOpNotRecordedAsSuccess(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("audit-bad-checksum")
	// 故意传错 checksum ⇒ 写面校验失败（400）。
	rr := env.upload(t, "alice", "bad.txt", body, sha256Hex([]byte("别的")), 0)
	if rr.Code == http.StatusOK {
		t.Fatalf("错误 checksum 不应 200, got %d", rr.Code)
	}
	for _, row := range env.audits {
		if row.object == "bad.txt" && row.result == auditResultSuccess {
			t.Fatalf("失败上传不得记为成功: %+v", row)
		}
	}
}

// TestAuditScenario_MultiFileDistinctRows 批量/多次操作逐条留痕（不得合并成一条）。
func TestAuditScenario_MultiFileDistinctRows(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	for _, name := range []string{"m1.txt", "m2.txt", "m3.txt"} {
		body := []byte("audit-" + name)
		if rr := env.upload(t, "alice", name, body, sha256Hex(body), 0); rr.Code != http.StatusOK {
			t.Fatalf("上传 %s 应 200, got %d: %s", name, rr.Code, rr.Body.String())
		}
	}
	n := 0
	for _, row := range env.audits {
		if row.action == "upload" && row.result == auditResultSuccess {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("3 次上传应留 3 条 upload 审计, got %d: %+v", n, env.audits)
	}
}
