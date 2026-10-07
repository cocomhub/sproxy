// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_audit_integrity_test.go 钉住下载阶段审计行对完整性判定结果的承载（#743 完整性
// 校验管道接入审计）：download span 的 End（成功/含 damaged 放行完成）与 Fail（失败）
// 双分支都在 Meta 写入 integrity_status（""/verified/damaged/unverified）+ integrity_sames
// （重下一致次数）。Fail 分支对 errIntegrityPermanent（两次一致仍异常）把状态归一为
// damaged——must-pass 阻断路径 task.IntegrityStatus 未置（放行标记不写），此处补全供审计。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"golang.org/x/sync/semaphore"
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

// integrityDecisionRows 返回 Type=resource + Step="integrity" 的逐次决策审计行
// （checkDownloadIntegrity 各裁决分支经 audit.From(dlCtx).Log 写入）。
func integrityDecisionRows(rows []audit.Row) []audit.Row {
	var out []audit.Row
	for i := range rows {
		if rows[i].Type == audit.TypeResource && rows[i].Step == "integrity" {
			out = append(out, rows[i])
		}
	}
	return out
}

// integrityDecisionsOf 汇总各行 decision Meta 值（供 assertHasIntegrityDecision 报错展示）。
func integrityDecisionsOf(rows []audit.Row) []string {
	var out []string
	for i := range rows {
		if m, ok := rows[i].Meta.(map[string]any); ok {
			out = append(out, metaString(m, "decision"))
		}
	}
	return out
}

// assertHasIntegrityDecision 断言逐次决策审计行中存在指定 decision 值。
func assertHasIntegrityDecision(t *testing.T, rows []audit.Row, want string) {
	t.Helper()
	for i := range rows {
		if m, ok := rows[i].Meta.(map[string]any); ok && metaString(m, "decision") == want {
			return
		}
	}
	t.Fatalf("应含 integrity 决策 decision=%q，实际 decisions: %v", want, integrityDecisionsOf(rows))
}

// findIntegrityDecisionRow 返回指定 decision 的首条决策行（无则 nil）。
func findIntegrityDecisionRow(rows []audit.Row, decision string) *audit.Row {
	for i := range rows {
		if m, ok := rows[i].Meta.(map[string]any); ok && metaString(m, "decision") == decision {
			return &rows[i]
		}
	}
	return nil
}

// TestCloudTask_Audit_IntegrityDecision_Released：默认放行 damaged 路径——download End 行
// Meta 携带 integrity_status=damaged + integrity_decision=released（消除「已释放损坏」与
// 「强制阻断」二义，A5-I2）；逐次决策审计行含 retry（attempt1 语义异常）与 release
// （attempt2 checksum 一致仍异常 → permanent → 放行），且 release 行带 integrity_checker
// （ImageChecker.Kind()="image/*"）与 integrity_check_ms（单独计时的校验耗时）。
func TestCloudTask_Audit_IntegrityDecision_Released(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	srv, _ := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "damaged" {
		t.Fatalf("前置：应 completed+damaged，got %q+%q", task.Status, task.IntegrityStatus)
	}

	rows := waitAuditRows(t, mgr, task.ID)
	dlRow := findAuditRowByType(rows, audit.TypeDownload)
	if dlRow == nil {
		t.Fatalf("应含 download 行，实际 %+v", rows)
	}
	meta, _ := dlRow.Meta.(map[string]any)
	if got := metaString(meta, "integrity_status"); got != "damaged" {
		t.Fatalf("download 行 integrity_status 应为 damaged，got %q", got)
	}
	if got := metaString(meta, "integrity_decision"); got != "released" {
		t.Fatalf("默认放行 download 行 integrity_decision 应为 released，got %q", got)
	}
	if got := metaInt64(meta, "integrity_sames"); got != 2 {
		t.Fatalf("download 行 integrity_sames 应为 2，got %d", got)
	}
	// 逐次决策审计行：retry（attempt 1 语义异常）/ release（attempt 2 permanent 放行）。
	decRows := integrityDecisionRows(rows)
	assertHasIntegrityDecision(t, decRows, "retry")
	assertHasIntegrityDecision(t, decRows, "release")
	rel := findIntegrityDecisionRow(decRows, "release")
	if rel == nil {
		t.Fatal("应含 release 决策行")
	}
	rmeta, _ := rel.Meta.(map[string]any)
	if got := metaString(rmeta, "integrity_checker"); got != "image/*" {
		t.Fatalf("release 决策行 integrity_checker 应为 image/*，got %q", got)
	}
	if got := metaInt64(rmeta, "integrity_check_ms"); got < 0 {
		t.Fatalf("release 决策行应带 integrity_check_ms（独立计时的校验耗时），got %d", got)
	}
}

// TestCloudTask_Audit_IntegrityDecision_Blocked：must-pass 阻断实例——download Fail 行
// Meta 把 integrity_status 归一为 damaged + integrity_decision=blocked（与 released 区分）；
// 逐次决策行含 block（permanent + must-pass 阻断）。
func TestCloudTask_Audit_IntegrityDecision_Blocked(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	srv, _ := corruptServe(t)

	task, err := mgr.SubmitAndStart("url", srv.URL, "bad.png", int64(len(corruptPNGF)), t.Context(), "", TaskParams{Save: true, IntegrityMustPass: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "failed" {
		t.Fatalf("must-pass 完整性异常应失败，got %q", task.Status)
	}

	rows := waitAuditRows(t, mgr, task.ID)
	dlRow := findAuditRowByType(rows, audit.TypeDownload)
	if dlRow == nil {
		t.Fatalf("应含 download 行，实际 %+v", rows)
	}
	meta, _ := dlRow.Meta.(map[string]any)
	if got := metaString(meta, "integrity_status"); got != "damaged" {
		t.Fatalf("阻断 Fail 行 integrity_status 应归一 damaged，got %q", got)
	}
	if got := metaString(meta, "integrity_decision"); got != "blocked" {
		t.Fatalf("must-pass 阻断 integrity_decision 应为 blocked，got %q", got)
	}
	decRows := integrityDecisionRows(rows)
	assertHasIntegrityDecision(t, decRows, "retry")
	assertHasIntegrityDecision(t, decRows, "block")
}

// TestCloudTask_Audit_IntegrityUnverified：内存配额超限跳过校验——download 行 End status=
// unverified（不误判 damaged），integrity_decision 留空（非 damaged）；逐次决策行独立记录
// decision=skip + reason=mem_quota + checker 类型（A5-I1-4 单独审计行）。
func TestCloudTask_Audit_IntegrityUnverified(t *testing.T) {
	t.Parallel()
	mgr := newIntegrityTestMgr(t, 3)
	mgr.checkMemSem = semaphore.NewWeighted(1 << 20) // 1 MiB
	mgr.checkMemMax = 1 << 20
	// 1000×1000 合法 PNG → EstimateMem=4MiB > 1MiB 配额 → overQuote 跳过校验 unverified。
	payload := largePNG(t, 1000, 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	task, err := mgr.SubmitAndStart("url", srv.URL, "big.png", int64(len(payload)), t.Context(), "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if task.Status != "completed" || task.IntegrityStatus != "unverified" {
		t.Fatalf("超配额应放行 completed+unverified，got %q+%q", task.Status, task.IntegrityStatus)
	}

	rows := waitAuditRows(t, mgr, task.ID)
	dlRow := findAuditRowByType(rows, audit.TypeDownload)
	if dlRow == nil {
		t.Fatalf("应含 download 行，实际 %+v", rows)
	}
	meta, _ := dlRow.Meta.(map[string]any)
	if got := metaString(meta, "integrity_status"); got != "unverified" {
		t.Fatalf("download 行 integrity_status 应为 unverified，got %q", got)
	}
	if got := metaString(meta, "integrity_decision"); got != "" {
		t.Fatalf("unverified 非 damaged，integrity_decision 应留空，got %q", got)
	}
	// 跳过路径独立审计行：skip + reason=mem_quota（A5-I1-4）。
	decRows := integrityDecisionRows(rows)
	skip := findIntegrityDecisionRow(decRows, "skip")
	if skip == nil {
		t.Fatalf("应含 skip 决策行，实际 decisions: %v", integrityDecisionsOf(decRows))
	}
	skmeta, _ := skip.Meta.(map[string]any)
	if got := metaString(skmeta, "reason"); got != "mem_quota" {
		t.Fatalf("skip 决策行 reason 应为 mem_quota，got %q", got)
	}
	if got := metaString(skmeta, "integrity_checker"); got != "image/*" {
		t.Fatalf("skip 行 integrity_checker 应为 image/*，got %q", got)
	}
}
