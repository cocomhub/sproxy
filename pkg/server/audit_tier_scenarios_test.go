// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_tier_scenarios_test.go 补设计 §9.1「卷管理」中的冷热分层动作：
// `volume_tier_downgrade`（hot→cold 迁移）与 `volume_tier_promote`（冷读回hot）。
//
// 两者都是**数据搬迁**（改变对象所在卷）⇒ 必须可追溯；驱动方式：直接调用 `scanOnce()`
// （周期循环只是定时包装），再对冷层对象发起一次读触发晋升。

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_TierDowngrade 超阈值文件被降级 ⇒ 一条 volume_tier_downgrade。
func TestAuditScenario_TierDowngrade(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	vols := []VolumeConfig{
		{Name: "hot", Type: "local", Root: filepath.Join(base, "hot"), Tier: "hot"},
		{Name: "cold", Type: "local", Root: filepath.Join(base, "cold"), Tier: "cold"},
	}
	// tier 扫描使用匿名 owner（见 tier_manager.go 的 v1 说明）⇒ actor 取空。
	url, h, _ := newVolumesAPIServer(t, "", vols, func(c *Config) {
		c.TierPolicy = TierPolicyConfig{Interval: time.Minute, MinSizeHot: 1}
	})
	attachTestAudit(t, h)

	body := []byte("cold-candidate")
	if st, resp := uploadFile(t, url, "tier.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	m := newTierManager(h, time.Minute)
	m.scanOnce()

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "volume_tier_downgrade"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("缺少 volume_tier_downgrade 审计行（跨卷迁移必须可追溯 ✗）")
	}
	if ev := entries[len(entries)-1]; ev.Result != "success" {
		t.Errorf("result = %q, want success", ev.Result)
	}

	// ② `volume_tier_promote`（冷读回迁）需要**完整下载路由 + owner ACL 视图**才能命中
	//    `resolveTierPromote`（本 harness 的匿名 owner 在下载面解析不到该对象 ⇒ 404）。
	//    该动作记档为后续项（见 docs/testing/audit-scenarios.md §3）。
}
