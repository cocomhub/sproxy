// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_volume_scenarios_test.go 补设计 §9.1「卷管理」类审计场景的第一个缺口：
// `volume_move`（跨卷移动）必须产生**可查询**的审计行，且 actor/object/result 正确 ——
// **成功与失败都要留痕**（§9.1 要求 denied/error 也记录）。
//
// 背景：此前 `volume_move` 只有成功路径埋点，ACL 403 / 源不存在 404 / 并发 409 / 容量 507
// 等失败路径**一条审计都没有** ✗（本文件引入了 `auditVolumeMoveIfFailed` 单一补充点）。
// 缺口清单见 docs/testing/audit-scenarios.md。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// newVolumeAuditServer 起多卷 API harness 并接上**真实审计服务**（journal + ring index）。
//
// 装配形态与生产一致（`setupAudit` 同构），只是目录在 TempDir；因此断言走的是
// `/api/audit` 使用的同一读入口（`readAuditEvents`），而不是测试专用旁路 ✓。
func newVolumeAuditServer(t *testing.T) (string, *Handlers, string) {
	t.Helper()
	base := t.TempDir()
	vols := []VolumeConfig{
		{Name: "vol-a", Type: "local", Root: filepath.Join(base, "a")},
		{Name: "vol-b", Type: "local", Root: filepath.Join(base, "b")},
	}
	url, h, _ := newVolumesAPIServer(t, "admin", vols, nil)

	attachTestAudit(t, h) // 与生产同构的审计接线（收敛到唯一助手）
	return url, h, base
}

// TestAuditScenario_VolumeMove 跨卷移动成功 ⇒ 1 条 volume_move（actor=操作者、result=success）。
func TestAuditScenario_VolumeMove(t *testing.T) {
	t.Parallel()
	url, h, _ := newVolumeAuditServer(t)

	body := []byte("audit-move")
	if st, resp := uploadFile(t, url, "mv-audit.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != 200 {
		t.Fatalf("上传源文件失败 status=%d body=%s", st, resp)
	}
	moveSuccess(t, url, "vol-a", "vol-b", "mv-audit.txt")

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "volume_move"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("跨卷移动后应有 volume_move 审计行（埋点缺失或读面不通 ✗）")
	}
	ev := entries[len(entries)-1]
	if ev.Action != "volume_move" {
		t.Fatalf("action = %q, want volume_move", ev.Action)
	}
	if ev.Actor != "admin" {
		t.Errorf("actor = %q, want admin（审计必须记录真实操作者）", ev.Actor)
	}
	if ev.Result != "success" {
		t.Errorf("result = %q, want success", ev.Result)
	}
	if !strings.Contains(ev.Detail, "vol-a") || !strings.Contains(ev.Detail, "vol-b") {
		t.Errorf("detail 应含 from→to 卷名, got %q", ev.Detail)
	}
}

// TestAuditScenario_VolumeMoveDenied 失败的移动也要留痕（result != success）。
func TestAuditScenario_VolumeMoveDenied(t *testing.T) {
	t.Parallel()
	url, h, _ := newVolumeAuditServer(t)

	// 源文件不存在 ⇒ move 失败（404）。
	if status, _ := moveVolume(t, url, "vol-a", "vol-b", "no-such-file.txt"); status == 200 {
		t.Fatalf("move 不存在的文件应失败, got 200")
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "volume_move"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("失败的 move 也应留痕（volume_move 失败埋点缺失 ✗）")
	}
	if got := entries[len(entries)-1].Result; got == "success" {
		t.Fatalf("失败路径 result 不能记 success, got %q", got)
	}
	if !strings.Contains(entries[len(entries)-1].Detail, "失败") {
		t.Errorf("失败行 detail 应说明失败, got %q", entries[len(entries)-1].Detail)
	}
}
