// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

// feed_edge_test.go 覆盖 feed.go 的渲染边界（结构断言已由 feed_test.go 覆盖）：
//
//  1. 空列表仍输出合法 RSS 2.0 / Atom 1.0（阅读器兼容；不出现空 <item>/<entry>）。
//  2. 标题/描述/link 含 XML 元字符与引号 → 全部转义，解析后可无损还原。
//  3. 噪音条目先过滤后截断：debounced/skipped 不占 feed_max 配额。

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRenderRSS2_EmptyEntries 空列表：XML 声明 + channel 元信息齐全、无 item 元素。
func TestRenderRSS2_EmptyEntries(t *testing.T) {
	t.Parallel()
	meta := FeedMeta{
		Title:   "sproxy 通知",
		Link:    "https://example.com/api/notify/feed",
		Desc:    "最近通知",
		Updated: time.Unix(0, 0).UTC(),
	}
	body, err := RenderRSS2(nil, meta)
	if err != nil {
		t.Fatalf("RenderRSS2 空列表: %v", err)
	}
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Fatalf("缺少 XML 声明: %q", body)
	}
	if strings.Contains(string(body), "<item>") {
		t.Fatalf("空列表不应输出 item: %s", body)
	}
	var doc struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel struct {
			Title         string `xml:"title"`
			Link          string `xml:"link"`
			Description   string `xml:"description"`
			LastBuildDate string `xml:"lastBuildDate"`
			Items         []struct {
				Title string `xml:"title"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("空列表 XML 不可解析: %v", err)
	}
	if doc.Version != "2.0" {
		t.Fatalf("rss version = %q, want 2.0", doc.Version)
	}
	if doc.Channel.Title != meta.Title || doc.Channel.Link != meta.Link || doc.Channel.Description != meta.Desc {
		t.Fatalf("channel 元信息不符: %+v", doc.Channel)
	}
	if len(doc.Channel.Items) != 0 {
		t.Fatalf("空列表 item 数 = %d, want 0", len(doc.Channel.Items))
	}
	if _, err := time.Parse(time.RFC1123Z, doc.Channel.LastBuildDate); err != nil {
		t.Fatalf("lastBuildDate 非 RFC1123Z (%q): %v", doc.Channel.LastBuildDate, err)
	}
}

// TestRenderAtom_EmptyEntries 空列表：xmlns 与 feed 元信息齐全、无 entry 元素。
func TestRenderAtom_EmptyEntries(t *testing.T) {
	t.Parallel()
	meta := FeedMeta{
		Title:   "sproxy 通知",
		Link:    "https://example.com/api/notify/feed?format=atom",
		Desc:    "最近通知",
		Updated: time.Unix(0, 0).UTC(),
	}
	body, err := RenderAtom(nil, meta)
	if err != nil {
		t.Fatalf("RenderAtom 空列表: %v", err)
	}
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Fatalf("缺少 XML 声明: %q", body)
	}
	if strings.Contains(string(body), "<entry>") {
		t.Fatalf("空列表不应输出 entry: %s", body)
	}
	var doc struct {
		XMLName  xml.Name `xml:"feed"`
		XMLNS    string   `xml:"xmlns,attr"`
		Title    string   `xml:"title"`
		ID       string   `xml:"id"`
		Subtitle string   `xml:"subtitle"`
		Updated  string   `xml:"updated"`
		Entries  []struct {
			ID string `xml:"id"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("空列表 XML 不可解析: %v", err)
	}
	if doc.XMLNS != "http://www.w3.org/2005/Atom" {
		t.Fatalf("xmlns = %q", doc.XMLNS)
	}
	if doc.Title != meta.Title || doc.ID != meta.Link || doc.Subtitle != meta.Desc {
		t.Fatalf("feed 元信息不符: %+v", doc)
	}
	if len(doc.Entries) != 0 {
		t.Fatalf("空列表 entry 数 = %d, want 0", len(doc.Entries))
	}
	if _, err := time.Parse(time.RFC3339Nano, doc.Updated); err != nil {
		t.Fatalf("feed updated 非 RFC3339Nano (%q): %v", doc.Updated, err)
	}
}

// TestRenderRSS2_EscapesReservedChars 标题/描述/link 中的 XML 元字符必须转义，
// 解析后与原始字符串完全一致（防 feed 注入）。
func TestRenderRSS2_EscapesReservedChars(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	const detail = `<script>alert("x")</script> & more`
	entry := HistoryEntry{
		TS: base, Action: "upload", Object: `a<b>&".txt`,
		Channel: "wecom", Status: "sent", Detail: detail,
	}
	meta := FeedMeta{
		Title:   `sproxy & "通知"`,
		Link:    "https://example.com/api/notify/feed?a=1&b=2",
		Desc:    "<desc>",
		Updated: base,
	}
	body, err := RenderRSS2([]HistoryEntry{entry}, meta)
	if err != nil {
		t.Fatalf("RenderRSS2: %v", err)
	}
	if strings.Contains(string(body), "<script>") {
		t.Fatalf("正文未转义（存在裸标签）: %s", body)
	}
	var doc struct {
		Channel struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			Items       []struct {
				Title       string `xml:"title"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("转义后 XML 不可解析: %v", err)
	}
	if doc.Channel.Title != meta.Title || doc.Channel.Link != meta.Link || doc.Channel.Description != meta.Desc {
		t.Fatalf("channel 元信息往返失真: %+v", doc.Channel)
	}
	if len(doc.Channel.Items) != 1 {
		t.Fatalf("item 数 = %d, want 1", len(doc.Channel.Items))
	}
	if want := FeedItemTitle(entry); doc.Channel.Items[0].Title != want {
		t.Fatalf("item title 往返失真: %q != %q", doc.Channel.Items[0].Title, want)
	}
	if doc.Channel.Items[0].Description != detail {
		t.Fatalf("item description 往返失真: %q != %q", doc.Channel.Items[0].Description, detail)
	}
}

// TestRenderAtom_EscapesReservedChars Atom 摘要中的 XML 元字符必须转义且可无损还原。
func TestRenderAtom_EscapesReservedChars(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	const detail = `<img src=x onerror="boom"> & "quoted"`
	entry := HistoryEntry{
		TS: base, Action: "delete", Object: `dir/<file>&.txt`,
		Channel: "email", Status: "failed", Detail: detail,
	}
	meta := FeedMeta{
		Title:   "sproxy & Atom",
		Link:    "https://example.com/api/notify/feed?format=atom&x=1",
		Desc:    "<subtitle>",
		Updated: base,
	}
	body, err := RenderAtom([]HistoryEntry{entry}, meta)
	if err != nil {
		t.Fatalf("RenderAtom: %v", err)
	}
	if strings.Contains(string(body), "<img src=") {
		t.Fatalf("摘要未转义（存在裸标签）: %s", body)
	}
	var doc struct {
		Title    string `xml:"title"`
		Subtitle string `xml:"subtitle"`
		Entries  []struct {
			Title   string `xml:"title"`
			Summary string `xml:"summary"`
			Link    struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("转义后 XML 不可解析: %v", err)
	}
	if doc.Title != meta.Title || doc.Subtitle != meta.Desc {
		t.Fatalf("feed 元信息往返失真: %+v", doc)
	}
	if len(doc.Entries) != 1 {
		t.Fatalf("entry 数 = %d, want 1", len(doc.Entries))
	}
	if want := FeedItemTitle(entry); doc.Entries[0].Title != want {
		t.Fatalf("entry title 往返失真: %q != %q", doc.Entries[0].Title, want)
	}
	if doc.Entries[0].Summary != detail {
		t.Fatalf("entry summary 往返失真: %q != %q", doc.Entries[0].Summary, detail)
	}
	if doc.Entries[0].Link.Href != meta.Link {
		t.Fatalf("entry link = %q, want %q", doc.Entries[0].Link.Href, meta.Link)
	}
}

// TestFeedEntries_FilteredNoiseDoesNotConsumeQuota 噪音条目先过滤后截断，不占 feed_max 配额。
func TestFeedEntries_FilteredNoiseDoesNotConsumeQuota(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0).UTC()
	var hist []HistoryEntry
	for i := range 10 {
		status := "debounced"
		if i%2 == 0 {
			status = "sent"
		}
		hist = append(hist, HistoryEntry{
			TS:     base.Add(-time.Duration(i) * time.Second),
			Action: "upload", Object: fmt.Sprintf("f%d", i),
			Channel: "wecom", Status: status,
		})
	}
	got := FeedEntries(hist, 3)
	if len(got) != 3 {
		t.Fatalf("应截断到 3 条（噪音不占配额），got %d", len(got))
	}
	for _, e := range got {
		if e.Status == "debounced" || e.Status == "skipped" {
			t.Fatalf("噪音条目不应出现在 feed: %v", got)
		}
	}
	if got[0].Object != "f0" || got[2].Object != "f4" {
		t.Fatalf("应保留最近在前的前 3 条 sent（f0,f2,f4），got %v", got)
	}
}
