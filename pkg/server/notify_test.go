// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// notify_test.go 验证通知中心框架（roadmap P0 通知中心）：
//  1. 规则路由：action glob（* / upload / delete）→ channels 命中。
//  2. 去抖：同 action+object 在窗口内只发一次；窗口外恢复（再触发再发）。
//  3. 历史：记录成功/失败 + 去抖跳过的条目（有界 200）。
//  4. 重试：渠道失败 → 指数退避重试 3 次后失败进历史。
//  5. wecom/serverchan 渠道：真实 HTTP POST（httptest mock 校验载荷）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestNotifyEndpoints /api/notify/history 与 /api/notify/test（经 h 装配）。
func TestNotifyEndpoints(t *testing.T) {
	t.Parallel()
	// 装配带通知中心的服务（wecom 渠道 → mock webhook）。
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()
	url, _, _ := newTestServer(t, func(c *Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []NotifyRule{{Action: "*", Channels: []string{"wecom"}}}
		c.Notify.Channels.Wecom.Webhook = srv.URL
	})
	// 无凭据（allowInsecureLoopback）→ localMux 隧道内层或 authMiddleware……
	// newTestServer 是 noAuth（allowInsecureLoopback）→ /api/notify/history 应可读。
	resp, err := http.Get(url + "/api/notify/history")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "entries") {
		t.Fatalf("history: status=%d body=%q", resp.StatusCode, body[:min(len(body), 60)])
	}
	// POST test（自检 wecom 渠道）。
	req, _ := http.NewRequest(http.MethodPost, url+"/api/notify/test", nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	resp2, err := cl.Do(req)
	if err != nil {
		t.Fatalf("POST test: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(body2), `"wecom":"ok"`) {
		t.Fatalf("test: status=%d body=%q", resp2.StatusCode, body2)
	}
}
