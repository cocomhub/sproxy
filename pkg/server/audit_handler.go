// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// maxAuditListLimit 是 GET /api/audit 的 limit 上限（超过 clamp 到该值）。
const maxAuditListLimit = 500

// defaultAuditListLimit 是 limit 非法（非整数/负数）时的回落默认值。
const defaultAuditListLimit = 100

// auditResponse 是 GET /api/audit 的响应体。
// Events 按时间倒序（最新在前）；Total 为本次返回条数（经 limit 截断后的实际条数）。
type auditResponse struct {
	Events []AuditEvent `json:"events"`
	Total  int          `json:"total"`
}

// auditFilterFromQuery 把 query 参数映射为统一过滤条件（唯一映射点，两端点共用）。
//   - action：**以 "." 结尾 ⇒ 前缀族**（如 "ai." 命中 "ai.summarize"），否则精确相等；
//   - actor / mesh：精确相等；
//   - timeKey 指定时间参数名（列表 since / 导出 after_ts）。
//
// 返回 (filter, ok)：时间解析失败 ⇒ ok=false（调用方回 400）。
func auditFilterFromQuery(q map[string][]string, timeKey string) (audit.EntryFilter, bool) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	f := audit.EntryFilter{Action: get("action"), Actor: get("actor"), Mesh: get("mesh")}
	if s := get(timeKey); s != "" {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return f, false
		}
		f.Since = ts
	}
	return f, true
}

// readAuditEvents 是读面的**唯一实现**：优先全量落盘历史（合规段族），未装配则回落内存索引。
//   - newestFirst=true（列表）：按时间倒序并应用 limit；
//   - newestFirst=false（导出）：按时间正序全量（供 collector 顺序回放）。
func (h *Handlers) readAuditEvents(f audit.EntryFilter, limit int, newestFirst bool) []AuditEvent {
	if h.auditJournal != nil {
		return h.readAuditFromJournal(f, limit, newestFirst)
	}
	if h.auditSvc != nil {
		lf := f
		if newestFirst {
			lf.Limit = limit
		}
		es := h.auditSvc.Recent(lf) // 索引最新在前
		if !newestFirst {
			slices.Reverse(es)
		}
		return auditEventsFromEntries(es)
	}
	return []AuditEvent{}
}

// readAuditFromJournal 读**全量落盘历史**（合规段族）并按需倒序 + limit。
func (h *Handlers) readAuditFromJournal(f audit.EntryFilter, limit int, newestFirst bool) []AuditEvent {
	es, err := h.auditJournal.Read(f) // 升序（(Node, Seq)）
	if err != nil {
		logger := h.auditLogger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("审计读取失败（返回空）", "error", err.Error())
		return []AuditEvent{}
	}
	if !newestFirst {
		return auditEventsFromEntries(es)
	}
	slices.Reverse(es)
	if limit > 0 && len(es) > limit {
		es = es[:limit]
	}
	return auditEventsFromEntries(es)
}

// auditHandler 处理 GET /api/audit——Web UI 审计日志查看面板。
//
// 设计要点：
//   - 双注册：主 mux 走 authMiddleware；localMux（隧道内层）—— 审计是浏览器隧道模式下的
//     用户面操作，隧道内层必须可达（鉴权/可见性收敛见设计 §2.5/§5.7，属 M1-11/M1-15）。
//   - 未启用审计（auditSvc/auditJournal 皆 nil）时返回 200 + 空 events + total 0（不 404）。
//
// query 参数：limit（默认 100，>500 clamp）、action/actor/mesh、since（RFC3339，非法 ⇒ 400）。
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

	f, ok := auditFilterFromQuery(q, "since")
	if !ok {
		http.Error(w, "since 参数非法，需为 RFC3339 时间（如 2026-09-01T12:00:00Z）", http.StatusBadRequest)
		return
	}

	events := h.readAuditEvents(f, limit, true)
	sendJSONResponse(w, auditResponse{Events: events, Total: len(events)}, http.StatusOK)
}

// auditExportHandler 处理 GET /api/audit/export——审计日志导出（JSON 数组，按 TS 升序）。
//
// 与 /api/audit 同一读面实现（readAuditEvents），导出为全量正序（运维面：CLI/脚本/collector）。
// query 参数：action/actor/mesh（同 /api/audit）、after_ts（RFC3339，非法 ⇒ 400）。
func (h *Handlers) auditExportHandler(w http.ResponseWriter, r *http.Request) {
	f, ok := auditFilterFromQuery(r.URL.Query(), "after_ts")
	if !ok {
		http.Error(w, "after_ts 参数非法，需为 RFC3339 时间（如 2026-09-01T12:00:00Z）", http.StatusBadRequest)
		return
	}
	sendJSONResponse(w, h.readAuditEvents(f, 0, false), http.StatusOK)
}
