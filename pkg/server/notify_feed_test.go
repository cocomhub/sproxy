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
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestFeedEntries_FiltersNoise debounced/skipped 排除、sent/failed 保留、最近在前。
func TestFeedEntries_FiltersNoise(t *testing.T) {
	t.Parallel()
	base := time.Now()
	hist := []historyEntry{
		{TS: base.Add(-1 * time.Second), Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent"},
		{TS: base.Add(-2 * time.Second), Action: "upload", Object: "a.txt", Channel: "wecom", Status: "debounced"},
		{TS: base.Add(-3 * time.Second), Action: "delete", Object: "b.txt", Channel: "serverchan", Status: "skipped"},
		{TS: base.Add(-4 * time.Second), Action: "delete", Object: "b.txt", Channel: "serverchan", Status: "failed"},
	}
	got := feedEntries(hist, 10)
	if len(got) != 2 {
		t.Fatalf("应保留 2 条（sent+failed），got %d: %v", len(got), histStatuses(got))
	}
	if got[0].Status != "sent" || got[1].Status != "failed" {
		t.Fatalf("顺序应为 sent,failed（最近在前），got %v", histStatuses(got))
	}
}

// TestFeedEntries_Truncate maxN 截断生效（保留最近在前的前 N 条）。
func TestFeedEntries_Truncate(t *testing.T) {
	t.Parallel()
	base := time.Now()
	hist := make([]historyEntry, 25)
	for i := range hist {
		hist[i] = historyEntry{TS: base.Add(-time.Duration(i) * time.Second), Action: "upload", Object: "f", Channel: "wecom", Status: "sent"}
	}
	got := feedEntries(hist, 20)
	if len(got) != 20 {
		t.Fatalf("截断到 20，got %d", len(got))
	}
	if !got[0].TS.After(got[len(got)-1].TS) {
		t.Fatalf("应保持最近在前顺序")
	}
	if feedEntries(hist, 0) != nil {
		t.Fatalf("maxN<=0 应返回 nil")
	}
}

// TestRenderRSS2_Structure channel 标题/条目数/pubDate RFC1123Z/guid 唯一。
func TestRenderRSS2_Structure(t *testing.T) {
	t.Parallel()
	base := time.Now().Truncate(time.Second)
	entries := []historyEntry{
		{TS: base, Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent", Detail: "ok"},
		{TS: base, Action: "upload", Object: "a.txt", Channel: "serverchan", Status: "sent"}, // 同刻多渠道
		{TS: base.Add(-time.Minute), Action: "delete", Object: "b.txt", Channel: "wecom", Status: "failed", Detail: "boom"},
	}
	meta := feedMeta{Title: "sproxy 通知", Link: "https://example.com/api/notify/feed", Desc: "最近通知", Updated: base}
	body, err := renderRSS2(entries, meta)
	if err != nil {
		t.Fatalf("renderRSS2: %v", err)
	}
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Fatalf("缺少 XML 声明")
	}
	var doc struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			Items []struct {
				Title   string `xml:"title"`
				PubDate string `xml:"pubDate"`
				GUID    string `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("解析 RSS: %v", err)
	}
	if doc.Version != "2.0" || doc.Channel.Title != meta.Title {
		t.Fatalf("rss version=%q title=%q", doc.Version, doc.Channel.Title)
	}
	if doc.Channel.Link != meta.Link {
		t.Fatalf("channel link = %q, want %q", doc.Channel.Link, meta.Link)
	}
	if len(doc.Channel.Items) != 3 {
		t.Fatalf("item 数 = %d, want 3", len(doc.Channel.Items))
	}
	seen := map[string]bool{}
	for _, it := range doc.Channel.Items {
		if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
			t.Fatalf("pubDate 非 RFC1123Z: %q", it.PubDate)
		}
		if it.GUID == "" || seen[it.GUID] {
			t.Fatalf("guid 缺失或重复: %q", it.GUID)
		}
		seen[it.GUID] = true
		// guid 含 channel 分量：同刻多渠道不撞（变异保护点）。
		if !strings.Contains(it.GUID, "wecom") && !strings.Contains(it.GUID, "serverchan") {
			t.Fatalf("guid 应含 channel 分量: %q", it.GUID)
		}
	}
}

// TestRenderAtom_Structure xmlns 命名空间/updated RFC3339Nano/id 唯一。
func TestRenderAtom_Structure(t *testing.T) {
	t.Parallel()
	base := time.Now().Truncate(time.Millisecond)
	entries := []historyEntry{
		{TS: base, Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent", Detail: "ok"},
		{TS: base, Action: "upload", Object: "a.txt", Channel: "serverchan", Status: "sent"}, // 同刻多渠道
	}
	meta := feedMeta{Title: "sproxy 通知", Link: "https://example.com/api/notify/feed", Desc: "最近通知", Updated: base}
	body, err := renderAtom(entries, meta)
	if err != nil {
		t.Fatalf("renderAtom: %v", err)
	}
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Fatalf("缺少 XML 声明")
	}
	var doc struct {
		XMLName xml.Name `xml:"feed"`
		XMLNS   string   `xml:"xmlns,attr"`
		Title   string   `xml:"title"`
		ID      string   `xml:"id"`
		Updated string   `xml:"updated"`
		Entries []struct {
			Title   string `xml:"title"`
			ID      string `xml:"id"`
			Updated string `xml:"updated"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("解析 Atom: %v", err)
	}
	if doc.XMLNS != "http://www.w3.org/2005/Atom" {
		t.Fatalf("xmlns = %q", doc.XMLNS)
	}
	if doc.Title != meta.Title || doc.ID != meta.Link {
		t.Fatalf("feed title=%q id=%q", doc.Title, doc.ID)
	}
	if _, err := time.Parse(time.RFC3339Nano, doc.Updated); err != nil {
		t.Fatalf("feed updated 非 RFC3339Nano: %q", doc.Updated)
	}
	if len(doc.Entries) != 2 {
		t.Fatalf("entry 数 = %d, want 2", len(doc.Entries))
	}
	seen := map[string]bool{}
	for _, e := range doc.Entries {
		if _, err := time.Parse(time.RFC3339Nano, e.Updated); err != nil {
			t.Fatalf("entry updated 非 RFC3339Nano: %q", e.Updated)
		}
		if e.ID == "" || seen[e.ID] {
			t.Fatalf("id 缺失或重复: %q", e.ID)
		}
		seen[e.ID] = true
	}
}

// feedStub 构造带 N 条 sent 历史的桩通知中心（测试用）。
func feedStub(now time.Time, n int) *NotifyCenter {
	nc := NewNotifyCenter(NotifyConfig{Enabled: true}, nil)
	nc.mu.Lock()
	for i := range n {
		nc.addHistory(AuditEvent{Action: "upload", Object: "f", Result: "success", TS: now.Add(-time.Duration(i) * time.Second)}, "wecom", "sent", "")
	}
	nc.mu.Unlock()
	return nc
}

// TestFeedTokenGate feed_token 非空：无/错 token → 401 空 body；对 token → 200。
func TestFeedTokenGate(t *testing.T) {
	t.Parallel()
	const tok = "feed-secret"
	nc := feedStub(time.Now(), 1)
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
	nc := feedStub(time.Now(), 5)
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
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// format=atom → Content-Type application/atom+xml。
	recA := httptest.NewRecorder()
	h.notifyFeedHandler(recA, httptest.NewRequest(http.MethodGet, "/api/notify/feed?format=atom", nil))
	if recA.Code != 200 {
		t.Fatalf("atom: status=%d body=%q", recA.Code, recA.Body.String())
	}
	if ct := recA.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/atom+xml") {
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
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/rss+xml") {
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
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/atom+xml") {
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
