// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"slices"
	"strconv"
	"time"
)

// maxAuditListLimit 是 GET /api/audit 的 limit 上限（超过 clamp 到该值）。
const maxAuditListLimit = 500

// defaultAuditListLimit 是 limit 非法（非整数/负数）时的回落默认值。
const defaultAuditListLimit = 100

// auditResponse 是 GET /api/audit 的响应体。
// Events 按时间倒序（最新在前）；Total 为本次返回条数（=len(events)，经 ring
// Recent 的 limit 截断后的实际条数，非全量命中数）。
// 事件 TS 经 Go time.Time 默认 JSON 序列化为 RFC3339Nano 字符串。
type auditResponse struct {
	Events []AuditEvent `json:"events"`
	Total  int          `json:"total"`
}

// auditHandler 处理 GET /api/audit——Web UI 审计日志查看面板。
//
// 设计要点：
//   - 双注册：主 mux 走 authMiddleware（SproxySig/APIKey 认证）；localMux（隧道
//     内层）裸注册（隧道加密即认证，与 /api/shares、/api/stats 同模式）。审计是
//     浏览器隧道模式下的用户面操作，隧道内层必须可达。
//   - ring 为 nil（audit.buffer_size=0 关闭）时返回 200 + 空 events + total 0
//     （不 404——Web UI 直接渲染空表）。
//
// query 参数：
//   - limit：非负整数；默认 100；>500 clamp 到 500；非法（非整数/负）回落默认 100。
//   - action / actor / mesh：精确相等过滤（空字段不过滤）。
//   - since：RFC3339 时间；解析失败返回 400。
func (h *Handlers) auditHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := defaultAuditListLimit
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxAuditListLimit {
		limit = maxAuditListLimit
	}

	f := AuditFilter{
		Action: q.Get("action"),
		Actor:  q.Get("actor"),
		Mesh:   q.Get("mesh"),
	}
	if s := q.Get("since"); s != "" {
		since, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "since 参数非法，需为 RFC3339 时间（如 2026-09-01T12:00:00Z）", http.StatusBadRequest)
			return
		}
		f.Since = since
	}

	var events []AuditEvent
	if h.auditRing != nil {
		events = h.auditRing.Recent(limit, f)
	} else {
		events = []AuditEvent{}
	}

	sendJSONResponse(w, auditResponse{Events: events, Total: len(events)}, http.StatusOK)
}

// auditExportHandler 处理 GET /api/audit/export——审计日志导出（JSON 数组，按 TS 升序）。
//
// 与 /api/audit 的双注册模式一致：主 mux 走 authMiddleware（SproxySig/APIKey 认证），
// localMux（隧道内层）裸注册（隧道加密即认证）。导出为运维面（CLI/脚本/日志 collector
// 消费），不做游标分页——导出量受 ring 容量上限约束，天然有界。
//
// query 参数（与 /api/audit 同款语义）：
//   - action / actor：精确相等过滤（空字段不过滤）。
//   - after_ts：RFC3339 时间，仅导出该时刻之后（TS.After）的事件；解析失败返回 400。
//
// 未启用审计（ring nil）时返回空数组 200（与 /api/audit 一致，不 404）。
func (h *Handlers) auditExportHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := AuditFilter{
		Action: q.Get("action"),
		Actor:  q.Get("actor"),
	}
	if s := q.Get("after_ts"); s != "" {
		since, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "after_ts 参数非法，需为 RFC3339 时间（如 2026-09-01T12:00:00Z）", http.StatusBadRequest)
			return
		}
		f.Since = since
	}

	// 导出全量（不带 limit）——ring 容量有界（默认 2048），全量导出即完整历史。
	// Recent 返回最新在前（倒序）；导出契约按 TS 升序（时间正序）输出。
	var events []AuditEvent
	if h.auditRing != nil {
		events = h.auditRing.Recent(h.auditRing.Capacity(), f)
	} else {
		events = []AuditEvent{}
	}

	// 倒序 → 升序：Recent 从最新往前遍历，反转为时间正序（与 /api/audit 的倒序
	// 展示不同——导出供日志 collector 顺序回放，正序更自然）。
	slices.Reverse(events)

	sendJSONResponse(w, events, http.StatusOK)
}
