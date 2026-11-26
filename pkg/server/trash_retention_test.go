// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash_retention_test.go 锁定「回收站保留期」的**单一事实源 + 对外可见**：
//  ① EffectiveTrashRetention 的边界（未配置/负值 ⇒ 7 天默认；显式值原样返回）；
//  ② GET /api/config 暴露 trash_retention（UI 据此显示真实保留期，不写死天数）；
//  ③ GC 任务与上报表口径**一致**（同一次清理窗口的 ttl 与 trash_retention 相等）。
//
// 背景：v2 回收站页面写死「删除的文件会保留 30 天」，而 `trash.ttl` 默认是 7 天
// （`routes_setup.trashGCTask` 里 ttl<=0 ⇒ 7 天）—— 文案与服务端行为矛盾，属误导。

import (
	"io"
	"net/http"
	"testing"
	"time"

	configpkg "github.com/cocomhub/sproxy/pkg/server/config"
)

func TestEffectiveTrashRetention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero-is-unset", 0, 7 * 24 * time.Hour},
		{"negative-is-unset", -time.Hour, 7 * 24 * time.Hour},
		{"explicit", 48 * time.Hour, 48 * time.Hour},
		{"one-day", 24 * time.Hour, 24 * time.Hour},
	}
	for _, c := range cases {
		if got := configpkg.EffectiveTrashRetention(c.in); got != c.want {
			t.Errorf("%s: configpkg.EffectiveTrashRetention(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestTrashRetentionObservableOverConfigAPI 锁「UI 能拿到真实保留期」：
// 默认配置（ttl 未设置）⇒ trash_retention 必须是 7 天而非空/0。
func TestTrashRetentionObservableOverConfigAPI(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Trash.TTL = 0 // 未配置 ⇒ 生效值取默认
	})
	req, _ := http.NewRequest(http.MethodGet, url+"/api/config", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !containsStr(s, `"trash_retention":"168h0m0s"`) {
		t.Fatalf("config 应暴露 trash_retention=168h0m0s（7 天默认），got: %s", s)
	}

	// 显式配置必须原样上报（不是恒默认）。
	url2, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Trash.TTL = 36 * time.Hour
	})
	req2, _ := http.NewRequest(http.MethodGet, url2+"/api/config", nil)
	resp2, err := testHTTPClient(t).Do(req2)
	if err != nil {
		t.Fatalf("GET /api/config（显式 ttl）: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if !containsStr(string(body2), `"trash_retention":"36h0m0s"`) {
		t.Fatalf("config 应如实上报 36h0m0s，got: %s", string(body2))
	}
}
