// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_volume_export_scenarios_test.go 补设计 §9.1「卷导出/导入」审计场景：
// `volume_export`（整卷打包外流）与 `volume_import`（外部数据落盘）—— 两个方向都是**数据面
// 大范围读写**，必须能回答「谁把哪个卷导出去了 / 谁把什么导进来了」。
//
// 断言经 `/api/audit/export`（查询与导出共用同一读面），复用既有导出/导入 harness。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// newAuditVolumeServer 与 `newTestServer` 同构，但**回环主体角色 = admin**（审计读面 admin-only）
// 且返回 `*Handlers` ⇒ 断言可走 `/api/audit` 的同一读入口（`readAuditEvents`）。
func newAuditVolumeServer(t *testing.T) (string, *Handlers) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	opts := RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
		InsecureLoopbackRole:  "admin",
	}
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, h
}

// TestAuditScenario_VolumeExportImport 导出侧记 volume_export、导入侧记 volume_import。
func TestAuditScenario_VolumeExportImport(t *testing.T) {
	t.Parallel()

	// ① 源端：上传 + 导出（导出动作本身即被审计）。
	srcURL, srcH := newAuditVolumeServer(t)
	manifest, entries := volumeExportSeedFiles(t, srcURL)

	// ② 目标端：导入同一份 tar。
	dstURL, dstH := newAuditVolumeServer(t)
	if status, res := postImport(t, dstURL, "", buildVolumeImportTar(t, manifest, entries), false); status != http.StatusOK {
		t.Fatalf("import status = %d, want 200", status)
	} else if !res.Success {
		t.Fatalf("import success = false: %s", res.Message)
	}

	// ③ 断言源端 volume_export。
	srcEvents := srcH.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	sawExport := false
	for _, ev := range srcEvents {
		if ev.Action != "volume_export" {
			continue
		}
		sawExport = true
		// 回环无认证装配下 actor 允许为空（ctx 无主体）；有 actor 时必须是操作者。
		if ev.Actor != "" && ev.Actor != testAccessKey {
			t.Errorf("volume_export actor = %q, want %q 或空（回环无认证）", ev.Actor, testAccessKey)
		}
		if ev.Object == "" {
			t.Error("volume_export object（卷名）不能为空")
		}
		if ev.Result != "success" {
			t.Errorf("volume_export result = %q, want success", ev.Result)
		}
	}
	if !sawExport {
		t.Error("缺少 volume_export 审计行（整卷外流必须可追溯 ✗）")
	}

	// ④ 断言目标端 volume_import。
	dstEvents := dstH.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	sawImport := false
	for _, ev := range dstEvents {
		if ev.Action != "volume_import" {
			continue
		}
		sawImport = true
		if ev.Actor != "" && ev.Actor != testAccessKey {
			t.Errorf("volume_import actor = %q, want %q 或空（回环无认证）", ev.Actor, testAccessKey)
		}
		if ev.Object == "" {
			t.Error("volume_import object（卷名）不能为空")
		}
	}
	if !sawImport {
		t.Error("缺少 volume_import 审计行（外部数据落盘必须可追溯 ✗）")
	}
}
