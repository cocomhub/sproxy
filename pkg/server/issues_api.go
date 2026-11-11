// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// issues_api.go 是「待处理事项」的 **HTTP 面**（唯一实现）。
//
// 用户裁定 2026-11-10：「支持 **web ui 人工查看**有哪些还未成功，提供**详细失败原因**便于人工
// 接入处理」。因此本文件提供：
//   - `GET  /api/issues`          列出事项（可按 status/volume/kind/未解决 筛选）；
//   - `GET  /api/issues/detail`   单条详情（含 Detail/LastError/Resolve 历史，供人工判断）；
//   - `POST /api/issues/resolve`  人工处理完成后标记已解决（**记录说明**，留审计）；
//   - `POST /api/issues/scan`      立即触发一次存储审查（把"发现残留"从"等下次启动"变为可主动触发）。
//
// 设计约定：
//   - 路径参数用 **query**（`?id=`）——双 mux 注册不解析 path 变量，避免自造路由匹配器；
//   - 响应一律经 `internal/httpjson`（R54 硬禁裸写 JSON）；
//   - 分类由装配层决定（R38）：本包只声明「这条路由在两端分属哪个分类」。

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/server/router"
)

// issuesRoutePrefix 是本域路由组前缀（R38 门禁依赖该常量的字面量）。
const issuesRoutePrefix = "/api"

// IssuesAPI 是事项的 HTTP 接口层（持有 IssueCenter 与可选的**手动扫描**回调）。
//
// 并发语义：路由在 `RegisterRoutes` 期注册（此时 center 尚未就绪——它依赖卷装配结果），
// 随后由装配根 `SetCenter`/`SetScanner` 注入；请求期只读 ⇒ 用 RWMutex 保护这次**一次性发布**，
// 既避免 data race，也不必让装配根倒序依赖。
type IssuesAPI struct {
	mu      sync.RWMutex
	center  *IssueCenter
	scanner func(ctx context.Context) ScanStats
}

// NewIssuesAPI 构造 HTTP 面（center 为 nil 时所有端点返回 503，便于"未装配"可观测）。
func NewIssuesAPI(center *IssueCenter) *IssuesAPI {
	return &IssuesAPI{center: center}
}

// SetCenter 发布事项中枢（装配根在卷装配完成后调用）。
func (a *IssuesAPI) SetCenter(c *IssueCenter) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.center = c
	a.mu.Unlock()
}

// SetScanner 注入"立即扫描"实现（nil = 不支持，端点回 501）。
func (a *IssuesAPI) SetScanner(fn func(ctx context.Context) ScanStats) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.scanner = fn
	a.mu.Unlock()
}

// snapshot 读取当前 center/scanner（请求期各取一次，保证同一次请求内一致）。
func (a *IssuesAPI) snapshot() (*IssueCenter, func(ctx context.Context) ScanStats) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.center, a.scanner
}

// Routes 返回本包拥有的全部路由（相对 `/api` 前缀 + 两端分类）。
//
// 分类说明：事项是**运维只读/审计面**（暴露失败原因与路径），故服务端走 `auth`（已认证即读）；
// 写操作（resolve/scan）标 `Mutating`，供集群只读副本的写面门拒绝。
func (a *IssuesAPI) Routes() []router.Route[http.HandlerFunc] {
	return []router.Route[http.HandlerFunc]{
		{Method: "GET", Path: "/issues", Server: router.ClassAuth, Local: router.ClassTunnel, H: a.handleList},
		{Method: "GET", Path: "/issues/detail", Server: router.ClassAuth, Local: router.ClassTunnel, H: a.handleDetail},
		{Method: "POST", Path: "/issues/resolve", Server: router.ClassAuth, Local: router.ClassTunnel, Mutating: true, H: a.handleResolve},
		{Method: "POST", Path: "/issues/scan", Server: router.ClassAuth, Local: router.ClassTunnel, Mutating: true, H: a.handleScan},
	}
}

// issuesListView 是列表响应（含状态汇总，便于 UI 直接渲染概览）。
type issuesListView struct {
	Total  int            `json:"total"`
	Stats  map[string]int `json:"stats"`
	Items  []issues.Item  `json:"items"`
	Status string         `json:"status,omitempty"`
	Volume string         `json:"volume,omitempty"`
	Kind   string         `json:"kind,omitempty"`
}

func (a *IssuesAPI) handleList(w http.ResponseWriter, r *http.Request) {
	c, _ := a.snapshot()
	if a == nil || c == nil || c.Manager() == nil {
		httpjson.SendError(w, http.StatusServiceUnavailable, "待处理事项未装配")
		return
	}
	q := r.URL.Query()
	f := issues.ListFilter{
		Status: issues.Status(strings.TrimSpace(q.Get("status"))),
		Kind:   issues.Kind(strings.TrimSpace(q.Get("kind"))),
		Volume: strings.TrimSpace(q.Get("volume")),
	}
	if v := strings.TrimSpace(q.Get("unresolved")); v == "1" || strings.EqualFold(v, "true") {
		f.UnresolvedOnly = true
	}
	items := c.Manager().List(f)
	stats := map[string]int{}
	for st, n := range c.Manager().Stats() {
		stats[string(st)] = n
	}
	httpjson.Send(w, http.StatusOK, issuesListView{
		Total: len(items), Stats: stats, Items: items,
		Status: string(f.Status), Volume: f.Volume, Kind: string(f.Kind),
	})
}

func (a *IssuesAPI) handleDetail(w http.ResponseWriter, r *http.Request) {
	c, _ := a.snapshot()
	if a == nil || c == nil || c.Manager() == nil {
		httpjson.SendError(w, http.StatusServiceUnavailable, "待处理事项未装配")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		httpjson.SendError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	it, ok := c.Manager().Get(id)
	if !ok {
		httpjson.SendError(w, http.StatusNotFound, "事项不存在")
		return
	}
	httpjson.Send(w, http.StatusOK, it)
}

func (a *IssuesAPI) handleResolve(w http.ResponseWriter, r *http.Request) {
	c, _ := a.snapshot()
	if a == nil || c == nil || c.Manager() == nil {
		httpjson.SendError(w, http.StatusServiceUnavailable, "待处理事项未装配")
		return
	}
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		httpjson.SendError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	note := strings.TrimSpace(q.Get("note"))
	if note == "" {
		httpjson.SendError(w, http.StatusBadRequest, "缺少 note（必须写明人工处置方式，供审计）")
		return
	}
	if !c.Manager().Resolve(id, note) {
		httpjson.SendError(w, http.StatusNotFound, "事项不存在")
		return
	}
	httpjson.Send(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// scanView 是手动扫描的响应。
type scanView struct {
	OK        bool      `json:"ok"`
	Volumes   int       `json:"volumes"`
	Skipped   int       `json:"skipped"`
	Nodes     int       `json:"nodes"`
	Reported  int       `json:"reported"`
	Truncated bool      `json:"truncated"`
	At        time.Time `json:"at"`
}

func (a *IssuesAPI) handleScan(w http.ResponseWriter, r *http.Request) {
	c, _ := a.snapshot()
	if a == nil || c == nil || c.Manager() == nil {
		httpjson.SendError(w, http.StatusServiceUnavailable, "待处理事项未装配")
		return
	}
	_, scanner := a.snapshot()
	if scanner == nil {
		httpjson.SendError(w, http.StatusNotImplemented, "本实例未装配存储审查目标（scanner 未注入）")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	st := scanner(ctx)
	httpjson.Send(w, http.StatusOK, scanView{
		OK: true, Volumes: st.Volumes, Skipped: st.Skipped, Nodes: st.Nodes,
		Reported: st.Reported, Truncated: st.Truncated, At: time.Now().UTC(),
	})
}
