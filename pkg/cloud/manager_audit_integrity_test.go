// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_audit_integrity_test.go 钉住下载阶段审计行对完整性判定结果的承载（#743 完整性
// 校验管道接入审计）：download span 的 End（成功/含 damaged 放行完成）与 Fail（失败）
// 双分支都在 Meta 写入 integrity_status（""/verified/damaged/unverified）+ integrity_sames
// （重下一致次数）。Fail 分支对 errIntegrityPermanent（两次一致仍异常）把状态归一为
// damaged——must-pass 阻断路径 task.IntegrityStatus 未置（放行标记不写），此处补全供审计。

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// waitAuditRows 轮询任务审计行非空（finalizeTaskAudit 在 executeDownload 收尾时把 sink
// 行拷入 task.Audit；同步路径已完成，轮询仅为防时序抖动，与既有审计用例一致）。
func waitAuditRows(t *testing.T, mgr *CloudDownloadManager, id string) []audit.Row {
	t.Helper()
	testutil.WaitFor(t, 10*time.Second, func() bool {
		snap, ok := mgr.SnapshotTask(id, "")
		return ok && len(snap.Audit) > 0
	}, func() string { return "等待 task.Audit 非空" })
	snap, ok := mgr.SnapshotTask(id, "")
	if !ok {
		t.Fatal("SnapshotTask not found")
	}
	return snap.Audit
}

// findAuditRowByType 返回首条指定 Type 的审计行（无则 nil）。
func findAuditRowByType(rows []audit.Row, typ audit.Type) *audit.Row {
	for i := range rows {
		if rows[i].Type == typ {
			return &rows[i]
		}
	}
	return nil
}

// metaString 读取 Meta 字符串键（未命中返回 ""）。
func metaString(meta map[string]any, key string) string {
	v, _ := meta[key].(string)
	return v
}

// metaInt64 宽容读取 Meta 数值键（内存 int/int64 与 FileSink 重新读盘的 JSON float64
// 均接受）；未命中/类型不符返回 -1（哨兵）。
func metaInt64(meta map[string]any, key string) int64 {
	switch n := meta[key].(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// TestCloudTask_Audit_DownloadEnd_IntegrityVerified：download span End 行 Meta 携带
// integrity_status=verified（权威匹配置位）+ integrity_sames=0（未触发重下）。
func TestCloudTask_Audit_DownloadEnd_IntegrityVerified(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 2)
	srv, _ := corruptServe(t)
	fd := &fakeAuthorityDownloader{url: srv.URL, authHash: "authority-hash"}
	reg := downloader.NewRegistry()
	reg.Register(downloader.Plugin[downloader.Downloader]{Name: "fakeauthority", Instance: fd, Priority: 10})
	mgr.registry = reg

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "verified" {
		t.Fatalf("前置：应 completed+verified，got %q+%q", task.Status, task.IntegrityStatus)
	}

	rows := waitAuditRows(t, mgr, task.ID)
	dlRow := findAuditRowByType(rows, audit.TypeDownload)
	if dlRow == nil {
		t.Fatalf("应含 Type=%q 下载行，实际 rows: %+v", audit.TypeDownload, rows)
	}
	if dlRow.Level != audit.LevelInfo {
		t.Fatalf("完成路径 download 行应为 Info 级，got %q", dlRow.Level)
	}
	meta, ok := dlRow.Meta.(map[string]any)
	if !ok {
		t.Fatalf("download Meta 应为 map，got %T", dlRow.Meta)
	}
	if got := metaString(meta, "integrity_status"); got != "verified" {
		t.Fatalf("download End 行 integrity_status 应为 verified，got %q", got)
	}
	if got := metaInt64(meta, "integrity_sames"); got != 0 {
		t.Fatalf("download End 行 integrity_sames 应为 0（未重下），got %d", got)
	}
}

// TestCloudTask_Audit_DownloadFail_IntegrityDamaged：IntegrityMustPass + 两次本地 checksum
// 一致仍异常（errIntegrityPermanent）→ 任务 failed，download span Fail 行 Meta 把
// integrity_status 归一为 damaged（task.IntegrityStatus 未置，Fail 分支补全）+
// integrity_sames==2（两次一致收敛次数）。
func TestCloudTask_Audit_DownloadFail_IntegrityDamaged(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	srv, _ := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true, IntegrityMustPass: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "failed" {
		t.Fatalf("IntegrityMustPass 完整性异常应失败，got %q (error=%q)", task.Status, task.Error)
	}
	if task.IntegrityStatus == "damaged" {
		t.Fatalf("前置：阻断路径不放行标记（task.IntegrityStatus 应空），got %q", task.IntegrityStatus)
	}

	rows := waitAuditRows(t, mgr, task.ID)
	dlRow := findAuditRowByType(rows, audit.TypeDownload)
	if dlRow == nil {
		t.Fatalf("应含 Type=%q 下载行，实际 rows: %+v", audit.TypeDownload, rows)
	}
	if dlRow.Level != audit.LevelError {
		t.Fatalf("失败路径 download 行应为 Error 级，got %q", dlRow.Level)
	}
	if !strings.Contains(dlRow.Err, "integrity") {
		t.Fatalf("Fail 行 Err 应含 integrity 关键字，got %q", dlRow.Err)
	}
	meta, ok := dlRow.Meta.(map[string]any)
	if !ok {
		t.Fatalf("download Meta 应为 map，got %T", dlRow.Meta)
	}
	if got := metaString(meta, "integrity_status"); got != "damaged" {
		t.Fatalf("Fail 行 integrity_status 应归一为 damaged，got %q", got)
	}
	if got := metaInt64(meta, "integrity_sames"); got != 2 {
		t.Fatalf("Fail 行 integrity_sames 应为 2（两次一致收敛），got %d", got)
	}
}
