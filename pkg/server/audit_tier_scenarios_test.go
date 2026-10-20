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
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_TierDowngradeAndPromote 降级（scanOnce）+ 冷读回迁（resolveTierPromote）各一条审计。
func TestAuditScenario_TierDowngradeAndPromote(t *testing.T) {
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

	// ② 冷读回迁：直接驱动读路径的单一决策点 `resolveTierPromote`（下载 handler 对它的
	//    调用只是「定位成功后才调用」的包装；本用例已把「对象在 cold 卷」这一前置条件摆好）。
	r := httptest.NewRequest(http.MethodGet, "/download?filename=tier.txt", nil)
	loc := &fileLocation{volumeName: "cold", tenant: h.volumeTenant("cold", "")}
	dp := h.resolveTierPromote(r, "", "tier.txt", "user/tier.txt", loc)
	if dp == nil {
		t.Fatal("冷层文件应被回迁到 hot 卷（resolveTierPromote 返回 nil ⇒ 未回迁 ✗）")
	}
	if dp.volName != "hot" {
		t.Errorf("回迁目标卷 = %q, want hot", dp.volName)
	}
	promoted := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "volume_tier_promote"}, 0, false)
	if len(promoted) == 0 {
		t.Fatal("缺少 volume_tier_promote 审计行（读时回迁改变对象所在卷，必须可追溯 ✗）")
	}
	if !strings.Contains(promoted[len(promoted)-1].Detail, "→") {
		t.Errorf("detail 应含 from→to 卷, got %q", promoted[len(promoted)-1].Detail)
	}
}
