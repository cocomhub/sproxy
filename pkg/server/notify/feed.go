// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

// notify_feed.go 是通知 RSS/Atom 订阅端点（roadmap 11.7-⑦）：
//
//   - GET /api/notify/feed 输出最近通知 RSS 2.0（默认）/ Atom（?format=atom），
//     数据源复用 NotifyCenter.History()（最近在前快照），零新增状态字段。
//   - 可选 token 门禁（feed_token）：?token=<t> 或 Authorization: Bearer <t>，
//     crypto/subtle 常量时间比较防时序侧信道；401 不区分「未提供/错误」（防枚举）。
//   - 纯标准库实现（encoding/xml），XML 全部经 xml 自动转义防注入。
//
// 过滤语义：排除 debounced/skipped 噪音条目（保留 sent/failed），截断到 feed_max
// （默认 20，上限 50）。空历史仍输出合法空 channel/feed XML（阅读器兼容）。

import (
	"encoding/xml"
	"fmt"
	"time"
)

// FeedMeta 是 feed 通道元信息（RSS channel / Atom feed 共用）。
type FeedMeta struct {
	Title   string    // 通道标题（如 "sproxy 通知"）
	Link    string    // 绝对 feed URL（阅读器回跳）
	Desc    string    // 通道描述
	Updated time.Time // 最近更新时间（RSS lastBuildDate / Atom updated）
}

// FeedEntries 过滤噪音条目（排除 status=debounced/skipped，保留 sent/failed）
// 并截断到 maxN（保留最近在前的 maxN 条；maxN<=0 返回 nil）。
func FeedEntries(hist []HistoryEntry, maxN int) []HistoryEntry {
	if maxN <= 0 {
		return nil
	}
	var out []HistoryEntry
	for _, e := range hist {
		if e.Status == "debounced" || e.Status == "skipped" {
			continue
		}
		out = append(out, e)
		if len(out) >= maxN {
			break
		}
	}
	return out
}

// FeedGUID 构造条目唯一 ID：同刻多渠道/多状态不撞（含 channel 分量）。
func FeedGUID(e HistoryEntry) string {
	return fmt.Sprintf("%d-%s-%s-%s-%s", e.TS.UnixNano(), e.Action, e.Object, e.Channel, e.Status)
}

// FeedItemTitle 构造条目标题（阅读器列表展示用）。
func FeedItemTitle(e HistoryEntry) string {
	return fmt.Sprintf("[%s] %s: %s", e.Status, e.Action, e.Object)
}

// ---- RSS 2.0 渲染 ----

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Items         []rssItem `xml:"item"`
}

type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

// RenderRSS2 渲染 RSS 2.0（pubDate=time.RFC1123Z；guid 唯一）。
func RenderRSS2(entries []HistoryEntry, meta FeedMeta) ([]byte, error) {
	doc := rssDoc{
		Version: "2.0",
		Channel: rssChannel{
			Title:         meta.Title,
			Link:          meta.Link,
			Description:   meta.Desc,
			LastBuildDate: meta.Updated.Format(time.RFC1123Z),
		},
	}
	for _, e := range entries {
		doc.Channel.Items = append(doc.Channel.Items, rssItem{
			Title:       FeedItemTitle(e),
			Link:        meta.Link,
			GUID:        FeedGUID(e),
			PubDate:     e.TS.Format(time.RFC1123Z),
			Description: e.Detail,
		})
	}
	b, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}

// ---- Atom 渲染 ----

type atomLink struct {
	Href string `xml:"href,attr"`
}

type atomEntry struct {
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Updated string   `xml:"updated"`
	Link    atomLink `xml:"link"`
	Summary string   `xml:"summary"`
}

type atomFeed struct {
	XMLName  xml.Name    `xml:"feed"`
	XMLNS    string      `xml:"xmlns,attr"`
	Title    string      `xml:"title"`
	ID       string      `xml:"id"`
	Link     atomLink    `xml:"link"`
	Updated  string      `xml:"updated"`
	Subtitle string      `xml:"subtitle"`
	Entries  []atomEntry `xml:"entry"`
}

// RenderAtom 渲染 Atom 1.0（xmlns 命名空间；updated=time.RFC3339Nano；id 唯一）。
func RenderAtom(entries []HistoryEntry, meta FeedMeta) ([]byte, error) {
	doc := atomFeed{
		XMLNS:    "http://www.w3.org/2005/Atom",
		Title:    meta.Title,
		ID:       meta.Link,
		Link:     atomLink{Href: meta.Link},
		Updated:  meta.Updated.Format(time.RFC3339Nano),
		Subtitle: meta.Desc,
	}
	for _, e := range entries {
		doc.Entries = append(doc.Entries, atomEntry{
			Title:   FeedItemTitle(e),
			ID:      FeedGUID(e),
			Updated: e.TS.Format(time.RFC3339Nano),
			Link:    atomLink{Href: meta.Link},
			Summary: e.Detail,
		})
	}
	b, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}
