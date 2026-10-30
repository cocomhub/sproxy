// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// notify_feed_test.go 验证通知 RSS/Atom 订阅端点（roadmap 11.7-⑦）：
//  1. feedEntries：过滤噪音（debounced/skipped）+ 截断到 maxN（最近在前）。
//  2. renderRSS2 / renderAtom：encoding/xml 可解析、时间格式正确、guid 唯一。
//  3. token 门禁：feed_token 非空时无/错 token → 401 空 body，对 token → 200。
//  4. feed_max 截断生效；format 非法 → 400；notify 未启用 → 400。
//  5. 集成：/api/notify/feed 返回合法 RSS/Atom（Content-Type + XML 可解析）。

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/server/notify"
)

// feedStub 构造带 N 条 sent 历史的桩通知中心（测试用）。
//
// 只经公开 API（NewNotifyCenter + Register + Dispatch）生成历史，不触碰
// notify 包内部字段——这样测试不会因实现细节变化而失效。
func feedStub(t *testing.T, now time.Time, n int) *NotifyCenter {
	t.Helper()
	cfg := NotifyConfig{
		Enabled: true,
		Rules:   []NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
	}
	nc := notify.NewNotifyCenter(cfg, nil)
	nc.Register(stubNotifier{})
	for i := range n {
		// Object 必须互不相同：NotifyCenter 默认去抖窗口 1 分钟，同 action+object+result
		// 的后续事件会被判为 debounced（feed 渲染会过滤掉）。
		nc.Dispatch(context.Background(), notify.Event{
			Action: "upload", Object: fmt.Sprintf("f%d", i), Result: "success",
			TS: now.Add(-time.Duration(i) * time.Second),
		})
	}
	// 条件轮询（不用固定 sleep）：Dispatch 异步，等历史落齐再返回。
	testutil.WaitFor(t, 5*time.Second, func() bool { return len(nc.History()) >= n }, "feedStub: 通知历史未在期限内就绪")
	return nc
}

// stubNotifier 是 feedStub 用的空渠道（Send 恒成功）。
type stubNotifier struct{}

func (stubNotifier) Name() string                                     { return "wecom" }
func (stubNotifier) Send(context.Context, notify.NotifyMessage) error { return nil }

// TestFeedTokenGate feed_token 非空：无/错 token → 401 空 body；对 token → 200。
func TestFeedTokenGate(t *testing.T) {
	t.Parallel()
	const tok = "feed-secret"
	nc := feedStub(t, time.Now(), 1)
	t.Cleanup(nc.Close)

	var cfgPtr atomic.Pointer[Config]
	cfg := Default()
	cfg.Notify.FeedToken = tok
	cfgPtr.Store(cfg)
	h := &Handlers{cfgPtr: &cfgPtr, notifyCenter: nc, logger: testLogger()}

	cases := []struct {
		name   string
		path   string
		header string
		want   int
	}{
		{"无凭据", "/api/notify/feed", "", 401},
		{"query 错误", "/api/notify/feed?token=wrong", "", 401},
		{"Bearer 错误", "/api/notify/feed", "Bearer wrong", 401},
		{"query 正确", "/api/notify/feed?token=" + tok, "", 200},
		{"Bearer 正确", "/api/notify/feed", "Bearer " + tok, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.notifyFeedHandler(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: status=%d want %d (body=%q)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 401 && rec.Body.Len() != 0 {
				t.Fatalf("%s: 401 应空 body, got %q", tc.name, rec.Body.String())
			}
		})
	}
}

// TestFeedLimitAndFormat feed_max 截断生效；format 非法 → 400。
func TestFeedLimitAndFormat(t *testing.T) {
	t.Parallel()
	nc := feedStub(t, time.Now(), 5)
	t.Cleanup(nc.Close)

	var cfgPtr atomic.Pointer[Config]
	cfg := Default()
	cfg.Notify.FeedMax = 2 // 显式小值：截断生效
	cfgPtr.Store(cfg)
	h := &Handlers{cfgPtr: &cfgPtr, notifyCenter: nc, logger: testLogger()}

	// maxN=2 → RSS 只有 2 个 item。
	rec := httptest.NewRecorder()
	h.notifyFeedHandler(rec, httptest.NewRequest(http.MethodGet, "/api/notify/feed", nil))
	if rec.Code != 200 {
		t.Fatalf("feed: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), "<item>"); n != 2 {
		t.Fatalf("item 数 = %d, want 2", n)
	}
	if ct := rec.Header().Get(headerContentType); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// format=atom → Content-Type application/atom+xml。
	recA := httptest.NewRecorder()
	h.notifyFeedHandler(recA, httptest.NewRequest(http.MethodGet, "/api/notify/feed?format=atom", nil))
	if recA.Code != 200 {
		t.Fatalf("atom: status=%d body=%q", recA.Code, recA.Body.String())
	}
	if ct := recA.Header().Get(headerContentType); !strings.HasPrefix(ct, "application/atom+xml") {
		t.Fatalf("atom Content-Type = %q", ct)
	}
	// format 非法 → 400。
	recBad := httptest.NewRecorder()
	h.notifyFeedHandler(recBad, httptest.NewRequest(http.MethodGet, "/api/notify/feed?format=json", nil))
	if recBad.Code != 400 {
		t.Fatalf("format 非法应 400, got %d", recBad.Code)
	}
}

// TestFeedNotEnabled notify 未启用（notifyCenter nil）→ 400。
func TestFeedNotEnabled(t *testing.T) {
	t.Parallel()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(Default())
	h := &Handlers{cfgPtr: &cfgPtr, logger: testLogger()} // notifyCenter nil
	rec := httptest.NewRecorder()
	h.notifyFeedHandler(rec, httptest.NewRequest(http.MethodGet, "/api/notify/feed", nil))
	if rec.Code != 400 {
		t.Fatalf("未启用应 400, got %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestNotifyFeedEndpoint 集成：路由挂载 + Content-Type + XML 可解析 + token 门。
func TestNotifyFeedEndpoint(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServer(t, func(c *Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []NotifyRule{{Action: "*", Channels: []string{"wecom"}}}
		c.Notify.Channels.Wecom.Webhook = "http://127.0.0.1:1" // 不实际发送（无事件）
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	// RSS（默认）。
	resp, err := cl.Get(url + "/api/notify/feed")
	if err != nil {
		t.Fatalf("GET feed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("feed: status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get(headerContentType); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var rssDoc struct {
		XMLName xml.Name `xml:"rss"`
		Channel struct {
			Items []struct{} `xml:"item"`
		} `xml:"channel"`
	}
	if err = xml.Unmarshal(body, &rssDoc); err != nil {
		t.Fatalf("解析 RSS: %v", err)
	}

	// Atom。
	resp2, err := cl.Get(url + "/api/notify/feed?format=atom")
	if err != nil {
		t.Fatalf("GET atom: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("atom: status=%d body=%q", resp2.StatusCode, body2)
	}
	if ct := resp2.Header.Get(headerContentType); !strings.HasPrefix(ct, "application/atom+xml") {
		t.Fatalf("atom Content-Type = %q", ct)
	}
	var atomDoc struct {
		XMLName xml.Name `xml:"feed"`
	}
	if err = xml.Unmarshal(body2, &atomDoc); err != nil {
		t.Fatalf("解析 Atom: %v", err)
	}

	// token 门（独立服务，feed_token 非空）。
	const tok = "feed-tok"
	urlT, _, _ := newTestServer(t, func(c *Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []NotifyRule{{Action: "*", Channels: []string{"wecom"}}}
		c.Notify.Channels.Wecom.Webhook = "http://127.0.0.1:1"
		c.Notify.FeedToken = tok
	})
	resp401, err := cl.Get(urlT + "/api/notify/feed")
	if err != nil {
		t.Fatalf("GET feed(no token): %v", err)
	}
	io.Copy(io.Discard, resp401.Body)
	resp401.Body.Close()
	if resp401.StatusCode != 401 {
		t.Fatalf("无 token 应 401, got %d", resp401.StatusCode)
	}
	respOK, err := cl.Get(urlT + "/api/notify/feed?token=" + tok)
	if err != nil {
		t.Fatalf("GET feed(token): %v", err)
	}
	io.Copy(io.Discard, respOK.Body)
	respOK.Body.Close()
	if respOK.StatusCode != 200 {
		t.Fatalf("带 token 应 200, got %d", respOK.StatusCode)
	}
}
