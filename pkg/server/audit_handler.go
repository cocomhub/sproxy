// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
	f := audit.EntryFilter{
		Action: get("action"), Actor: get("actor"), Mesh: get("mesh"),
		// 卷维度（评审 R13）：此前 Entry.Volume 未接线且过滤器无该维度。
		Volume: get("volume"),
	}
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
	hot := h.readAuditHot(ctx, f, limit, newestFirst)
	// 最新优先且热段已够数 ⇒ 更旧的冷段不可能进入结果 ⇒ 跳过（按需，不做无效 IO ✓）。
	skippable := newestFirst && limit > 0 && len(hot) >= limit
	var cold []audit.Entry
	if !skippable {
		cold = h.readAuditCold(ctx, f, limit, len(hot), newestFirst)
	}
	return auditEventsFromEntries(mergeAuditHotCold(cold, hot, limit, newestFirst))
}

// readAuditHot 读热段：最新优先走 `ReadTail`（新段优先、收满即停），否则 `ReadContext`（锁外解析）。
func (h *Handlers) readAuditHot(ctx context.Context, f audit.EntryFilter, limit int, newestFirst bool) []audit.Entry {
	var (
		es  []audit.Entry
		err error
	)
	if newestFirst && limit > 0 {
		es, err = h.auditJournal.ReadTail(ctx, f, limit)
	} else {
		es, err = h.auditJournal.ReadContext(ctx, f)
	}
	if err != nil {
		h.auditWarn("审计热段读取失败（忽略热段继续冷段）", err)
		return nil
	}
	return es
}

// readAuditCold 读冷段，并把**剩余预算**下推（0 = 不设限，用于导出/正序全量）。
func (h *Handlers) readAuditCold(ctx context.Context, f audit.EntryFilter, limit, hotLen int, newestFirst bool) []audit.Entry {
	coldFS := h.auditColdFS()
	if coldFS == nil {
		return nil
	}
	budget := 0
	if limit > 0 && limit-hotLen > 0 {
		budget = limit - hotLen
	}
	// 方向决定读法（评审 R13 正确性 + 有界性）：
	//   · newest-first + limit ⇒ **尾读**。`QueryCold` 逐段**从头**读且「收满即停」⇒ 最新段条目数
	//     超过预算时返回该段**最旧**的 N 条（最新条目被截掉）⇒ 结果集错误；
	//   · 其余（升序导出/不限量）⇒ 最旧优先扫描。
	if newestFirst {
		es, err := audit.ReadTailCold(ctx, coldFS, []string{""}, f, budget)
		if err != nil {
			h.auditWarn("审计冷段尾读失败（忽略冷段继续）", err)
			return nil
		}
		return es
	}
	es, err := audit.QueryCold(ctx, coldFS, []string{""}, f, budget)
	if err != nil {
		h.auditWarn("审计冷段读取失败（忽略冷段继续）", err)
		return nil
	}
	return es
}

// mergeAuditHotCold 归并热/冷（冷更旧）+ 倒序 + 截断。
func mergeAuditHotCold(cold, hot []audit.Entry, limit int, newestFirst bool) []audit.Entry {
	merged := make([]audit.Entry, 0, len(cold)+len(hot))
	merged = append(merged, cold...)
	merged = append(merged, hot...)
	if newestFirst {
		slices.Reverse(merged)
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// auditColdFS 返回冷段归档存储（`audit.archive_volume` 卷优先，其次 `audit.archive_dir`）。
//
// 与归档写侧**共用同一构造**（`newColdArchiveFS`）⇒ 读写看到同一布局；未配置归档 ⇒ nil
// （读面跳过冷段，零开销）。
func (h *Handlers) auditColdFS() syncpkg.FS {
	if h.cfgPtr == nil {
		return nil
	}
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return nil
	}
	logger := h.auditLogger
	if logger == nil {
		logger = slog.Default()
	}
	return newColdArchiveFS(cfg, h.volSet, logger)
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

// auditColdSegHandler 处理 GET /api/audit/cold/seg——按区间读取**已归档冷段**的一段字节
// （取证式随机读：管理员按需取任意区间，不必整段下载）。
//
// 参数：`path`（必须来自 ListCold 的白名单，防目录穿越）、`offset`、`length`
// （缺省 0 / 64KiB，上限 maxColdRangeBytes）。响应带 `X-Audit-Segment-Size` 与
// `Content-Range: bytes off-end-1/size` 便于客户端续读。
//
// 能力来源：`pkg/audit.OpenRange`（→ `sync.RangeReader`）—— 远端卷也只传输所需区间。
func (h *Handlers) auditColdSegHandler(w http.ResponseWriter, r *http.Request) {
	if !h.auditReadAllowed(r) {
		sendJSONResponse(w, map[string]string{"error": "审计读面仅限管理员"}, http.StatusForbidden)
		return
	}
	coldFS := h.auditColdFS()
	if coldFS == nil {
		sendJSONResponse(w, map[string]string{"error": "审计归档未启用"}, http.StatusNotFound)
		return
	}
	ctx := r.Context()
	segs, err := audit.ListCold(ctx, coldFS, "")
	if err != nil {
		h.auditWarn("审计冷段清单读取失败", err)
		http.Error(w, "冷段清单不可用", http.StatusInternalServerError)
		return
	}
	want := r.URL.Query().Get("path")
	var seg audit.ColdSegment
	found := false
	for _, s := range segs {
		if s.Path == want {
			seg, found = s, true
			break
		}
	}
	if !found {
		// 不在白名单 ⇒ 404（不区分「不存在」与「无权」，避免探测）。
		http.NotFound(w, r)
		return
	}
	off, n, ok := coldRangeParams(r, seg.Size)
	if !ok {
		http.Error(w, "offset/length 非法", http.StatusBadRequest)
		return
	}
	rc, oerr := audit.OpenRange(ctx, coldFS, seg, off, n)
	if oerr != nil {
		h.auditWarn("审计冷段区间读失败", oerr)
		http.Error(w, "冷段区间读失败", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Audit-Segment-Size", strconv.FormatInt(seg.Size, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, seg.Size))
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// maxColdRangeBytes 是单次冷段区间读的上限（防一次拉满整段）。
const maxColdRangeBytes = 4 << 20

// coldRangeParams 解析并夹紧 (offset,length) 到 [0,size)。
func coldRangeParams(r *http.Request, size int64) (off, n int64, ok bool) {
	q := r.URL.Query()
	off = 0
	if s := q.Get("offset"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, false
		}
		off = v
	}
	n = 64 << 10
	if s := q.Get("length"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			return 0, 0, false
		}
		n = v
	}
	if n > maxColdRangeBytes {
		n = maxColdRangeBytes
	}
	if off > size {
		off = size
	}
	if off+n > size {
		n = size - off
	}
	return off, n, true
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
