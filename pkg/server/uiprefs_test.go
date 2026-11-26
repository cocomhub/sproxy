// Copyright (C) 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestUIPrefs_DefaultDeleteMode 锁用户维度页面配置的「删除默认方式」语义：
//   - 未配置 ⇒ 安全默认 soft（进回收站）；
//   - PUT hard ⇒ 落盘并可回读；非法值 ⇒ 400 且**不改变**已有配置；
//   - 配置损坏/不存在 ⇒ 回落 soft（绝不因配置故障变成不可恢复删除）。
func TestUIPrefs_DefaultDeleteMode(t *testing.T) {
	t.Parallel()
	url, h := newEventTestServer(t, nil)
	attachTestAudit(t, h)
	client := testHTTPClient(t)

	get := func() (mode string) {
		resp, err := client.Get(url + "/api/ui/prefs")
		if err != nil {
			t.Fatalf("GET prefs: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET prefs status=%d", resp.StatusCode)
		}
		var out struct {
			Prefs UIPrefs `json:"prefs"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析: %v", derr)
		}
		return out.Prefs.Delete.DefaultMode
	}

	if got := get(); got != UIDeleteModeSoft {
		t.Fatalf("未配置时默认应为 soft（安全默认），got %q", got)
	}

	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, url+"/api/ui/prefs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT prefs: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := put(`{"delete":{"default_mode":"hard"}}`); code != http.StatusOK {
		t.Fatalf("PUT hard status=%d", code)
	}
	// 场景证据（R35-b）：直接读 handler 的审计服务内存索引，断言 ui_prefs_update 已发出。
	if h.auditSvc == nil {
		t.Fatal("测试夹具未装配审计服务")
	}
	found := false
	for _, e := range h.auditSvc.Recent(audit.EntryFilter{Action: "ui_prefs_update"}) {
		if e.Action == "ui_prefs_update" {
			found = true
		}
	}
	if !found {
		t.Fatal("审计中未记录 ui_prefs_update")
	}
	if got := get(); got != UIDeleteModeHard {
		t.Fatalf("回读默认方式 = %q，期望 hard", got)
	}

	// 非法值必须 400 且不改变既有配置（fail-closed）。
	if code := put(`{"delete":{"default_mode":"purge"}}`); code != http.StatusBadRequest {
		t.Fatalf("非法值应 400，got %d", code)
	}
	if got := get(); got != UIDeleteModeHard {
		t.Fatalf("非法 PUT 不得改变配置，got %q", got)
	}

	// 改回 soft。
	if code := put(`{"delete":{"default_mode":"SOFT"}}`); code != http.StatusOK {
		t.Fatalf("PUT SOFT（大小写不敏感）status=%d", code)
	}
	if got := get(); got != UIDeleteModeSoft {
		t.Fatalf("回读 = %q，期望 soft", got)
	}
}

// TestUIPrefs_NormalizeZero 锁 Config 零值归一：零值/未知值一律 soft（安全默认）。
func TestUIPrefs_NormalizeZero(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "weird", "HARD", "soft"} {
		got := UIPrefs{Delete: UIDeletePrefs{DefaultMode: in}}.normalize().Delete.DefaultMode
		want := UIDeleteModeSoft
		if in == "soft" {
			want = UIDeleteModeSoft
		}
		if got != want {
			t.Fatalf("normalize(%q) = %q want %q", in, got, want)
		}
	}
	if got := (UIPrefs{Delete: UIDeletePrefs{DefaultMode: "hard"}}).normalize().Delete.DefaultMode; got != UIDeleteModeHard {
		t.Fatalf("hard 应保留，got %q", got)
	}
}
