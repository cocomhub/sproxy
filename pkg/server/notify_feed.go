// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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
	"crypto/subtle"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// feedMeta 是 feed 通道元信息（RSS channel / Atom feed 共用）。
type feedMeta struct {
	Title   string    // 通道标题（如 "sproxy 通知"）
	Link    string    // 绝对 feed URL（阅读器回跳）
	Desc    string    // 通道描述
	Updated time.Time // 最近更新时间（RSS lastBuildDate / Atom updated）
}

// feedEntries 过滤噪音条目（排除 status=debounced/skipped，保留 sent/failed）
// 并截断到 maxN（保留最近在前的 maxN 条；maxN<=0 返回 nil）。
func feedEntries(hist []historyEntry, maxN int) []historyEntry {
	if maxN <= 0 {
		return nil
	}
	var out []historyEntry
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

// feedGUID 构造条目唯一 ID：同刻多渠道/多状态不撞（含 channel 分量）。
func feedGUID(e historyEntry) string {
	return fmt.Sprintf("%d-%s-%s-%s-%s", e.TS.UnixNano(), e.Action, e.Object, e.Channel, e.Status)
}

// feedItemTitle 构造条目标题（阅读器列表展示用）。
func feedItemTitle(e historyEntry) string {
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

// renderRSS2 渲染 RSS 2.0（pubDate=time.RFC1123Z；guid 唯一）。
func renderRSS2(entries []historyEntry, meta feedMeta) ([]byte, error) {
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
			Title:       feedItemTitle(e),
			Link:        meta.Link,
			GUID:        feedGUID(e),
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

// renderAtom 渲染 Atom 1.0（xmlns 命名空间；updated=time.RFC3339Nano；id 唯一）。
func renderAtom(entries []historyEntry, meta feedMeta) ([]byte, error) {
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
			Title:   feedItemTitle(e),
			ID:      feedGUID(e),
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

// feedBaseURL 构造请求的绝对 base URL（https 判定 + X-Forwarded-Proto 覆盖）。
func feedBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	}
	return scheme + "://" + r.Host
}

// feedMaxEntries 返回 feed 最大条目数（cfg 未配/<=0 → 20；>50 收敛到上限 50）。
func feedMaxEntries(cfg *Config) int {
	maxN := 20
	if cfg != nil && cfg.Notify.FeedMax > 0 {
		maxN = cfg.Notify.FeedMax
	}
	if maxN > 50 {
		maxN = 50
	}
	return maxN
}

// notifyFeedHandler 返回最近通知 RSS/Atom（GET /api/notify/feed?format=rss|atom）。
// feed_token 非空时强制 token 门禁（401 空 body）；notify 未启用 → 400。
func (h *Handlers) notifyFeedHandler(w http.ResponseWriter, r *http.Request) {
	if h.notifyCenter == nil {
		http.Error(w, "notify 未启用", http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	// token 门禁：feed_token 空 = 公开（默认零回归）；非空 = 必须携带
	// `?token=<t>` 或 `Authorization: Bearer <t>`（常量时间比较），否则 401
	// 空 body（不区分「未提供/错误」，防 token 枚举）。
	if cfg != nil && cfg.Notify.FeedToken != "" {
		want := []byte(cfg.Notify.FeedToken)
		got := []byte(r.URL.Query().Get("token"))
		if len(got) == 0 {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				got = []byte(strings.TrimPrefix(auth, "Bearer "))
			}
		}
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	// format 参数（默认 rss；非法值 400）。
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "rss"
	}
	if format != "rss" && format != "atom" {
		http.Error(w, "format 仅支持 rss|atom", http.StatusBadRequest)
		return
	}

	entries := feedEntries(h.notifyCenter.History(), feedMaxEntries(cfg))
	meta := feedMeta{
		Title:   "sproxy 通知",
		Link:    feedBaseURL(r) + "/api/notify/feed",
		Desc:    "最近通知",
		Updated: time.Now(),
	}
	if len(entries) > 0 {
		meta.Updated = entries[0].TS // 最新条目时间（历史最近在前）
	}

	var (
		body []byte
		err  error
		ct   string
	)
	if format == "atom" {
		body, err = renderAtom(entries, meta)
		ct = "application/atom+xml; charset=utf-8"
	} else {
		body, err = renderRSS2(entries, meta)
		ct = "application/rss+xml; charset=utf-8"
	}
	if err != nil {
		h.logger.Warn("渲染通知 feed 失败", "error", err)
		http.Error(w, "feed 渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(body)
}
