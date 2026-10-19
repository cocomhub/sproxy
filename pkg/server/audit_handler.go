// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
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
func (h *Handlers) readAuditEvents(ctx context.Context, f audit.EntryFilter, limit int, newestFirst bool) []AuditEvent {
	if h.auditJournal != nil {
		return h.readAuditFromJournal(ctx, f, limit, newestFirst)
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

// readAuditFromJournal 读**全量历史**：热段（Journal） **+** 冷段（已归档段），按需倒序 + limit。
//
// 冷段经 `audit.QueryCold` 读取（`pkg/sync.FS` + `RangeReader`/流式 ⇒ 远端/加密卷同样适用、
// 按需不整文件下载 ✓）；两段历史都是**升序**（冷 = 更旧，热 = 更新）⇒ 这里做一次归并，
// 使「在线热段」与「已归档冷段」共用**同一读取入口**（导出与查询不各写一套 ✗）。
func (h *Handlers) readAuditFromJournal(ctx context.Context, f audit.EntryFilter, limit int, newestFirst bool) []AuditEvent {
	hot, herr := h.auditJournal.Read(f) // 升序（(Node, Seq)）
	if herr != nil {
		h.auditWarn("审计热段读取失败（忽略热段继续冷段）", herr)
		hot = nil
	}
	var cold []audit.Entry
	// 最新优先且热段已够数 ⇒ 更旧的冷段不可能进入结果 ⇒ 跳过（按需，不做无效 IO ✓）。
	skippable := newestFirst && limit > 0 && len(hot) >= limit
	if !skippable {
		if dir := h.auditColdDir(); dir != "" {
			es, cerr := audit.QueryCold(ctx, newColdArchiveFS(dir, h.auditLogger), []string{""}, f, 0)
			if cerr != nil {
				h.auditWarn("审计冷段读取失败（忽略冷段继续）", cerr)
			} else {
				cold = es
			}
		}
	}
	merged := make([]audit.Entry, 0, len(cold)+len(hot))
	merged = append(merged, cold...) // 更旧
	merged = append(merged, hot...)
	if newestFirst {
		slices.Reverse(merged)
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return auditEventsFromEntries(merged)
}

// auditColdDir 返回冷段归档目录（`audit.archive_dir`；空 = 未启用归档）。
func (h *Handlers) auditColdDir() string {
	if h.cfgPtr == nil {
		return ""
	}
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return ""
	}
	return cfg.Audit.ArchiveDir
}

// auditWarn 记录审计读面告警（失败不打断读：审计读面 best-effort，但必须留痕 ✓）。
func (h *Handlers) auditWarn(msg string, err error) {
	logger := h.auditLogger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn(msg, "error", err.Error())
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
	if !h.auditReadAllowed(r) {
		sendJSONResponse(w, map[string]string{"error": "审计读面仅限管理员"}, http.StatusForbidden)
		return
	}
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

	events := h.readAuditEvents(r.Context(), f, limit, true)
	sendJSONResponse(w, auditResponse{Events: events, Total: len(events)}, http.StatusOK)
}

// auditExportHandler 处理 GET /api/audit/export——审计日志导出（JSON 数组，按 TS 升序）。
//
// 与 /api/audit 同一读面实现（readAuditEvents），导出为全量正序（运维面：CLI/脚本/collector）。
// query 参数：action/actor/mesh（同 /api/audit）、after_ts（RFC3339，非法 ⇒ 400）。
func (h *Handlers) auditExportHandler(w http.ResponseWriter, r *http.Request) {
	if !h.auditReadAllowed(r) {
		sendJSONResponse(w, map[string]string{"error": "审计读面仅限管理员"}, http.StatusForbidden)
		return
	}
	f, ok := auditFilterFromQuery(r.URL.Query(), "after_ts")
	if !ok {
		http.Error(w, "after_ts 参数非法，需为 RFC3339 时间（如 2026-09-01T12:00:00Z）", http.StatusBadRequest)
		return
	}
	sendJSONResponse(w, h.readAuditEvents(r.Context(), f, 0, false), http.StatusOK)
}
