// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_metrics_wiring_test.go 锁定评审 R13 P1：`audit.Metrics` 必须真正接入 `/metrics`。
//
// 背景：此前生产装配从未注入 `audit.Metrics` ⇒ 恒 `NullMetrics` ⇒ `audit_dropped`（**真正丢失**）
// /写失败/钩子 panic/轮转全部不可观测，而文档把它们当可观测契约。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestAuditMetrics_DroppedObservable 触发有界丢弃 ⇒ 计数器 +1 且 `/metrics` 暴露该指标。
func TestAuditMetrics_DroppedObservable(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Audit.MaxTotalBytes = 1 // 1 字节 ⇒ 任何条目都放不下 ⇒ 必然「有界丢弃」

	m := NewMetrics()
	svc, j := setupAuditServices(cfg, func() syncpkg.FS { return nil }, newAuditMetrics(m), testLogger())
	if svc == nil || j == nil {
		t.Fatalf("审计装配失败：svc=%v journal=%v", svc != nil, j != nil)
	}
	t.Cleanup(func() { _ = j.Close() })

	for range 5 {
		if err := svc.Emit(context.Background(), audit.Entry{
			Scope: "system", Kind: audit.KindEvent, Action: "drop_probe",
			Type: "sensitive", Result: "success", TS: time.Now(),
		}); err != nil {
			t.Fatalf("Emit 不应失败（审计 best-effort，绝不阻塞业务）: %v", err)
		}
	}
	if got := m.AuditDropped.Load(); got == 0 {
		t.Fatal("超总容量应计入 audit_dropped（真正的审计丢失必须可观测）")
	}

	// `/metrics` 必须真正暴露该指标（否则注入等于没做）。
	h := &Handlers{metrics: m}
	rec := httptest.NewRecorder()
	h.MetricsHandler(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"sproxy_audit_dropped_total", "sproxy_audit_evicted_total", "sproxy_audit_rotate_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 缺少 %s", want)
		}
	}
}
