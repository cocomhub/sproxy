// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

// TestFeedEntries_FiltersNoise debounced/skipped 排除、sent/failed 保留、最近在前。
func TestFeedEntries_FiltersNoise(t *testing.T) {
	t.Parallel()
	base := time.Now()
	hist := []HistoryEntry{
		{TS: base.Add(-1 * time.Second), Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent"},
		{TS: base.Add(-2 * time.Second), Action: "upload", Object: "a.txt", Channel: "wecom", Status: "debounced"},
		{TS: base.Add(-3 * time.Second), Action: "delete", Object: "b.txt", Channel: "serverchan", Status: "skipped"},
		{TS: base.Add(-4 * time.Second), Action: "delete", Object: "b.txt", Channel: "serverchan", Status: "failed"},
	}
	got := FeedEntries(hist, 10)
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
	hist := make([]HistoryEntry, 25)
	for i := range hist {
		hist[i] = HistoryEntry{TS: base.Add(-time.Duration(i) * time.Second), Action: "upload", Object: "f", Channel: "wecom", Status: "sent"}
	}
	got := FeedEntries(hist, 20)
	if len(got) != 20 {
		t.Fatalf("截断到 20，got %d", len(got))
	}
	if !got[0].TS.After(got[len(got)-1].TS) {
		t.Fatalf("应保持最近在前顺序")
	}
	if FeedEntries(hist, 0) != nil {
		t.Fatalf("maxN<=0 应返回 nil")
	}
}

// TestRenderRSS2_Structure channel 标题/条目数/pubDate RFC1123Z/guid 唯一。
func TestRenderRSS2_Structure(t *testing.T) {
	t.Parallel()
	base := time.Now().Truncate(time.Second)
	entries := []HistoryEntry{
		{TS: base, Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent", Detail: "ok"},
		{TS: base, Action: "upload", Object: "a.txt", Channel: "serverchan", Status: "sent"}, // 同刻多渠道
		{TS: base.Add(-time.Minute), Action: "delete", Object: "b.txt", Channel: "wecom", Status: "failed", Detail: "boom"},
	}
	meta := FeedMeta{Title: "sproxy 通知", Link: "https://example.com/api/notify/feed", Desc: "最近通知", Updated: base}
	body, err := RenderRSS2(entries, meta)
	if err != nil {
		t.Fatalf("RenderRSS2: %v", err)
	}
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Fatalf("缺少 XML 声明")
	}
	var doc struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel struct {
			Title string     `xml:"title"`
			Link  string     `xml:"link"`
			Items []rss2Item `xml:"item"`
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
	assertRSS2ItemsUniqueRFC1123Z(t, doc.Channel.Items)
}

// rss2Item 是 RSS channel item 的解析视图。
type rss2Item struct {
	Title   string `xml:"title"`
	PubDate string `xml:"pubDate"`
	GUID    string `xml:"guid"`
}

// assertRSS2ItemsUniqueRFC1123Z 断言每个条目的 pubDate 为 RFC1123Z、guid 唯一且含 channel 分量。
func assertRSS2ItemsUniqueRFC1123Z(t *testing.T, items []rss2Item) {
	t.Helper()
	seen := map[string]bool{}
	for _, it := range items {
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
	entries := []HistoryEntry{
		{TS: base, Action: "upload", Object: "a.txt", Channel: "wecom", Status: "sent", Detail: "ok"},
		{TS: base, Action: "upload", Object: "a.txt", Channel: "serverchan", Status: "sent"}, // 同刻多渠道
	}
	meta := FeedMeta{Title: "sproxy 通知", Link: "https://example.com/api/notify/feed", Desc: "最近通知", Updated: base}
	body, err := RenderAtom(entries, meta)
	if err != nil {
		t.Fatalf("RenderAtom: %v", err)
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
