// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// usage_report.go 是计量报告（roadmap 11.10-⑩）片 2 的**导出端点与装配**：
//
//   - GET /api/usage/report?owner=&from=&to=&format=csv|json：per-owner 周期用量
//     导出（owner 空 = 全部；格式默认 json）。
//     权限矩阵（复用 auth 语义）：`api_keys` 管理员/Bearer 或 SproxySig owner 自查询
//     ——只允许 owner 看自己或管理员看全部（admin 判定 = credentialRing 账号级
//     Key.Role==RoleAdmin，与 /api/credentials 同源）；越权 403。
//   - 错误处理：owner 不存在/无数据 → 200 + 空数组（报告语义不 404）；from/to
//     非法或缺失 → 400；format 非法 → 400；未启用（usage.enabled=false）→ 400。
//   - 数据流：upload/download 成功路径 → RecordUsageForOwner → usageStore 日桶累计
//     → 报告端点读 store 聚合（日/月桶求和）→ CSV/JSON 序列化（CSV 经 RFC 4180
//     转义，复用 usageCSVEscape）。
//   - 周期落盘：usage.interval > 0 时经统一调度器周期 Flush（零回归：0 = 关闭，
//     优雅停服 Close 仍 Flush 一次）；落盘失败仅记 Warn（内存仍可读，降级为不持久）。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// usageKindsUpload 是上传成功路径的用量 kind（usageStore 日桶 key）。
const usageKindUpload = "upload_bytes"

// usageKindsDownload 是下载成功路径的用量 kind。
const usageKindDownload = "download_bytes"

// usageEarliestDate 是报告缺省 from（全范围查询的下界，早于任何现实数据）。
const usageEarliestDate = "2000-01-01"

// usageReportResponse 是 GET /api/usage/report 的 JSON 响应体。
type usageReportResponse struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	Reports []usageSummary `json:"reports"`
}

// isUsageAdmin 判定请求主体是否为计量报告管理员（与 /api/credentials 同源：账号级
// Key.Role==RoleAdmin；api_keys 模式合成的最小 Principal 恒 user → 非管理员）。
func (h *Handlers) isUsageAdmin(actor string) bool {
	return actor != "" && h.getRole(actor) == "admin"
}

// usageReportAllowedOwner 解析报告查询目标 owner 并做权限裁决：
//   - 显式 owner 非空 → 管理员可查任意；普通主体只可查自己（否则 403）；
//   - owner 空（全部）→ 仅管理员可查（否则 403）。
//
// 返回 (目标 owner, 是否全部, 是否放行)。
func (h *Handlers) usageReportAllowedOwner(r *http.Request) (owner string, all, ok bool) {
	actor := ActorFrom(r.Context())
	reqOwner := strings.TrimSpace(r.URL.Query().Get("owner"))
	if reqOwner != "" {
		if reqOwner == actor || h.isUsageAdmin(actor) {
			return normalizeOwner(reqOwner), false, true
		}
		return "", false, false
	}
	if h.isUsageAdmin(actor) {
		return "", true, true
	}
	return "", false, false
}

// usageReportHandler 处理 GET /api/usage/report（主 mux 经 authMiddleware 保护；
// localMux 隧道内层裸注册——隧道加密即认证，owner 自查询语义与主面一致）。
func (h *Handlers) usageReportHandler(w http.ResponseWriter, r *http.Request) {
	if h.usageStore == nil {
		http.Error(w, "usage 未启用", http.StatusBadRequest)
		return
	}

	owner, all, ok := h.usageReportAllowedOwner(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// from/to：可省略（缺省全范围——报告语义宽松）；提供时必须 YYYY-MM-DD 且 from<=to。
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" {
		from = usageEarliestDate
	}
	if to == "" {
		to = time.Now().Format("2006-01-02")
	}
	fromT, ferr := time.Parse("2006-01-02", from)
	toT, terr := time.Parse("2006-01-02", to)
	if ferr != nil || terr != nil || fromT.After(toT) {
		http.Error(w, "from/to 非法：需为 YYYY-MM-DD 且 from<=to", http.StatusBadRequest)
		return
	}

	// format：默认 json；仅 csv|json。
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		http.Error(w, "format 仅支持 csv|json", http.StatusBadRequest)
		return
	}

	reports := h.usageReports(owner, all, from, to)
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="usage-report.csv"`)
		_, _ = w.Write([]byte(renderUsageCSV(reports)))
		return
	}
	sendJSONResponse(w, usageReportResponse{From: from, To: to, Reports: reports}, http.StatusOK)
}

// usageReports 按目标 owner 汇总报告：显式 owner → 单条；全部 → 磁盘扫描租户
// （与 verify/mirror 同源：listTenantIDs + anonymous 兜底）逐 owner 汇总。
// 无数据返回空切片（报告语义，非 404）。
func (h *Handlers) usageReports(owner string, all bool, from, to string) []usageSummary {
	if !all {
		sum := h.usageStore.Summary(owner, from, to)
		if len(sum.Kinds) == 0 {
			return []usageSummary{}
		}
		return []usageSummary{sum}
	}
	owners := h.listTenantIDs()
	if len(owners) == 0 {
		owners = []string{anonymousOwner}
	}
	if !slicesContains(owners, anonymousOwner) {
		owners = append(owners, anonymousOwner)
	}
	var out []usageSummary
	for _, o := range owners {
		sum := h.usageStore.Summary(o, from, to)
		if len(sum.Kinds) == 0 {
			continue
		}
		out = append(out, sum)
	}
	if len(out) == 0 {
		return []usageSummary{}
	}
	return out
}

// slicesContains 是 slices.Contains 的最小内联版（避免本文件引入 slices 依赖）。
func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// renderUsageCSV 把报告渲染为 CSV（RFC 4180：逗号/引号/换行经 usageCSVEscape）。
// 每 kind 一行：owner,from,to,days,kind,value（kind 固定顺序 upload→download→其余
// 按 kind 名排序，输出稳定便于 diff）。
func renderUsageCSV(reports []usageSummary) string {
	var b strings.Builder
	b.WriteString("owner,from,to,days,kind,value\r\n")
	for _, rep := range reports {
		kindOrder := make([]string, 0, len(rep.Kinds))
		if _, ok := rep.Kinds[usageKindUpload]; ok {
			kindOrder = append(kindOrder, usageKindUpload)
		}
		if _, ok := rep.Kinds[usageKindDownload]; ok {
			kindOrder = append(kindOrder, usageKindDownload)
		}
		for k := range rep.Kinds {
			if k != usageKindUpload && k != usageKindDownload {
				kindOrder = append(kindOrder, k)
			}
		}
		for _, k := range kindOrder {
			fmt.Fprintf(&b, "%s,%s,%s,%d,%s,%d\r\n",
				usageCSVEscape(rep.Owner), usageCSVEscape(rep.From), usageCSVEscape(rep.To),
				rep.Days, usageCSVEscape(k), rep.Kinds[k])
		}
	}
	return b.String()
}

// usagePersistDir 返回计量落盘目录（<默认卷根>/meta/usage；Root.Abs 只推导不创建，
// usageStore 懒建时 MkdirAll）。
func (h *Handlers) usagePersistDir() string {
	if h.globalRoot == nil {
		return ""
	}
	if abs, ok := h.globalRoot.Abs("meta/usage"); ok {
		return abs
	}
	return ""
}

// setupUsageReport 装配计量报告（RegisterRoutes 调用）：
//   - usage.enabled=false（缺省）→ 不装配（usageStore 保持 nil，端点 400 零回归）；
//   - 启用 → 懒建 usageStore（persistDir = <默认卷根>/meta/usage，载入历史）、
//     Metrics.usageRecorder 转发、scheduler 周期 Flush（interval>0）；
//   - 落盘失败仅记 Warn（内存仍可读，降级为不持久）。
func (h *Handlers) setupUsageReport(cfg *Config, log *slog.Logger) {
	if !cfg.Usage.Enabled {
		return
	}
	persistDir := h.usagePersistDir()
	if persistDir == "" {
		log.Warn("计量报告启用但无法解析落盘目录（默认卷根不可用），降级为纯内存", "usage_persist_dir", persistDir)
	}
	store := newUsageStore(persistDir, log.With("component", "usage"))
	h.usageStore = store
	if h.metrics != nil {
		h.metrics.SetUsageRecorder(func(owner, kind string, n int64) {
			store.RecordUsage(owner, kind, n)
		})
	}
	if cfg.Usage.Interval > 0 {
		if h.scheduler == nil {
			h.scheduler = NewScheduler(log.With("component", "scheduler"))
		}
		if regErr := h.scheduler.Register(Task{
			Name:     "usage-flush",
			Interval: cfg.Usage.Interval,
			Run: func(context.Context) {
				if err := store.Flush(); err != nil {
					log.Warn("用量周期落盘失败（内存仍可读）", "error", err.Error())
				}
			},
		}); regErr != nil {
			log.Error("usage-flush 任务注册失败，跳过周期落盘", "error", regErr.Error())
		}
	}
}
