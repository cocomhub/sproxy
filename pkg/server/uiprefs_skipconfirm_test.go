// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// uiprefs_skipconfirm_test.go 锁「删除类别的第二个配置项」：**跳过二次确认**。
//
// 语义：`delete.skip_confirm = true` 时页面上的删除不再弹确认框（危险操作一次性授权）。
// 默认 **false**（确认框开启）—— 按 AGENTS 7c 纪律，布尔配置必须默认 false，
// 且字段名必须表达「关闭某安全能力」（`skip_confirm` 而非 `confirm`），
// 这样「未配置」与「显式关闭」不会混淆。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

func TestUIPrefs_DeleteSkipConfirm(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	attachTestAudit(t, h)
	client := testHTTPClient(t)

	get := func() UIPrefs {
		t.Helper()
		resp, err := client.Get(url + "/api/ui/prefs")
		if err != nil {
			t.Fatalf("GET prefs: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Prefs UIPrefs `json:"prefs"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析: %v", derr)
		}
		return out.Prefs
	}
	put := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, url+"/api/ui/prefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT prefs: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 未配置 ⇒ 确认框开启（skip_confirm=false，安全默认）。
	if p := get(); p.Delete.SkipConfirm {
		t.Fatal("未配置时 skip_confirm 必须为 false（确认框开启）")
	}

	// 显式打开「跳过确认」。
	if code := put(`{"delete":{"skip_confirm":true}}`); code != http.StatusOK {
		t.Fatalf("PUT skip_confirm=true status=%d", code)
	}
	if p := get(); !p.Delete.SkipConfirm {
		t.Fatal("回读 skip_confirm 应为 true")
	}
	// 与 default_mode 互不影响（部分更新）。
	if p := get(); p.Delete.DefaultMode != UIDeleteModeSoft {
		t.Fatalf("改 skip_confirm 不应影响 default_mode，got %q", p.Delete.DefaultMode)
	}
	// 审计 detail 记录实际改动字段。
	found := false
	for _, e := range h.auditSvc.Recent(audit.EntryFilter{Action: "ui_prefs_update"}) {
		if strings.Contains(e.Detail, "delete.skip_confirm=true") {
			found = true
		}
	}
	if !found {
		t.Fatal("审计未记录 delete.skip_confirm=true")
	}

	// 显式关闭。
	if code := put(`{"delete":{"skip_confirm":false}}`); code != http.StatusOK {
		t.Fatalf("PUT skip_confirm=false status=%d", code)
	}
	if p := get(); p.Delete.SkipConfirm {
		t.Fatal("回读 skip_confirm 应为 false")
	}

	// 一次改两个字段。
	if code := put(`{"delete":{"default_mode":"hard","skip_confirm":true}}`); code != http.StatusOK {
		t.Fatalf("PUT 两字段 status=%d", code)
	}
	if p := get(); !p.Delete.SkipConfirm || p.Delete.DefaultMode != UIDeleteModeHard {
		t.Fatalf("两字段回读 = %+v", p.Delete)
	}

	// 空 delete 对象（既没 default_mode 也没 skip_confirm）⇒ 400，不落盘。
	before := get()
	if code := put(`{"delete":{}}`); code != http.StatusBadRequest {
		t.Fatalf("空 delete 应 400，got %d", code)
	}
	if after := get(); after != before {
		t.Fatalf("空 delete 改变了配置：before=%+v after=%+v", before, after)
	}
}

// TestUIPrefs_SkipConfirmNormalize 锁「配置文件里写了非法/深层零值」时的归一。
func TestUIPrefs_SkipConfirmNormalize(t *testing.T) {
	t.Parallel()
	// 零值 ⇒ false（确认开启）。
	if got := (UIPrefs{}).normalize(); got.Delete.SkipConfirm {
		t.Fatal("零值归一后 skip_confirm 必须 false")
	}
	// 显式 true 保留。
	if got := (UIPrefs{Delete: UIDeletePrefs{SkipConfirm: true}}).normalize(); !got.Delete.SkipConfirm {
		t.Fatal("显式 true 应保留")
	}
}
