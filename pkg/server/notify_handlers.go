// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/notify"
)

// notify_handlers.go 是通知域的 **HTTP 面**（历史/自检/feed 三个端点 + feed token 门禁）。
// 领域实现（NotifyCenter / 渠道 / feed 渲染）在 pkg/server/notify；本文件只做请求解析、
// 鉴权与响应写出——装配层依赖切片，切片不反向依赖装配层（R6）。

// ---- 运维端点 ----

// notifyHistoryHandler 返回通知历史（GET /api/notify/history?limit=N）。
func (h *Handlers) notifyHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if h.notifyCenter == nil {
		http.Error(w, "notify 未启用", http.StatusBadRequest)
		return
	}
	limit := 100
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	hist := h.notifyCenter.History()
	if len(hist) > limit {
		hist = hist[:limit]
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": hist})
}

// notifyTestHandler 触发渠道自检（POST /api/notify/test?channel=wecom）。
// 无 channel 参数 → 全部已注册渠道。返回各渠道测试结果。
func (h *Handlers) notifyTestHandler(w http.ResponseWriter, r *http.Request) {
	if h.notifyCenter == nil {
		http.Error(w, "notify 未启用", http.StatusBadRequest)
		return
	}
	channel := r.URL.Query().Get("channel")
	var names []string
	channels := h.notifyCenter.Channels()
	if channel != "" {
		if _, ok := channels[channel]; ok {
			names = []string{channel}
		} else {
			http.Error(w, "channel 不存在", http.StatusNotFound)
			return
		}
	} else {
		for name := range channels {
			names = append(names, name)
		}
	}

	results := map[string]string{}
	for _, name := range names {
		ch := channels[name]
		msg := NotifyMessage{Title: "[sproxy] 通知渠道自检", Text: "channel test", Object: "test", Action: "test"}
		if err := ch.Send(r.Context(), msg); err != nil {
			results[name] = "failed: " + err.Error()
		} else {
			results[name] = "ok"
		}
	}
	// 审计：通知渠道自检（可能外发真实消息 ⇒ 可追溯）。
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "notify_test", ObjectType: "notify", Object: strings.Join(names, ","), Result: AuditResultSuccess,
	})
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
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
	if !feedTokenAllowed(cfg, r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
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

	entries := notify.FeedEntries(h.notifyCenter.History(), feedMaxEntries(cfg))
	meta := notify.FeedMeta{
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
		body, err = notify.RenderAtom(entries, meta)
		ct = "application/atom+xml; charset=utf-8"
	} else {
		body, err = notify.RenderRSS2(entries, meta)
		ct = "application/rss+xml; charset=utf-8"
	}
	if err != nil {
		h.logger.Warn("渲染通知 feed 失败", "error", err)
		http.Error(w, "feed 渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, ct)
	_, _ = w.Write(body)
}

// feedTokenAllowed 校验 feed token 门禁：feed_token 非空时必须携带 `?token=<t>` 或
// `Authorization: Bearer <t>`（crypto/subtle 常量时间比较，防时序侧信道）；feed_token
// 空 = 公开（默认零回归）。
func feedTokenAllowed(cfg *Config, r *http.Request) bool {
	if cfg == nil || cfg.Notify.FeedToken == "" {
		return true
	}
	want := []byte(cfg.Notify.FeedToken)
	got := []byte(r.URL.Query().Get("token"))
	if len(got) == 0 {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = []byte(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
