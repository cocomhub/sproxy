// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// routes.go 声明本包拥有的**AI 洞察端点 URL 子路径**（相对前缀），由装配层的
// `router.Register` 一次注册到 srvMux / localMux —— 双 mux 不再各写一遍。
//
// 鉴权分类（Server / Local）是**装配层的决定**：本包只声明「这条路由在两端分属哪个分类」，
// 不提供 wrapper（wrapper 由装配层按分类注入）。
//
// summarize/tag 的 handler 需要装配态（`aiInsight` 与 ctx→owner 派生），装配层用闭包构造后
// 经 Endpoints 传入（P1-c 裁决：ctx→actor 不进领域包）。
//
// 门禁 R38 静态解析下面的字面量表与 RoutePrefix，与 route_classes.tsv 双向比对。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// RoutePrefix 是本包路由组前缀（R38 依赖该常量的字面量）。
const RoutePrefix = "/api/ai"

// Endpoints 是本组各操作在装配层的 handler（`*Handlers` 方法或其闭包）。
//
// H 用**命名结构体字段 + 函数类型**而非字符串键：字段名写错在编译期即失败，且
// 子包不持有装配态的 handler。
type Endpoints struct {
	Summarize    http.HandlerFunc // POST /api/ai/summarize
	Tag          http.HandlerFunc // POST /api/ai/tag
	Quota        http.HandlerFunc // GET /api/ai/quota
	Privacy      http.HandlerFunc // GET /api/ai/privacy
	PrivacyPurge http.HandlerFunc // POST /api/ai/privacy/purge
}

// Routes 返回本包拥有的全部路由（相对路径 + 两端分类）。
//
// 分类约定（与现状一致，改动须同步 route_classes.tsv）：
//   - 外部面（srvMux）：summarize/tag 未注册（ClassNone）；quota/privacy/privacy-purge
//     为 public 裸注册（handler 内按 owner 自查询，见 route_classes.tsv 既有分类）；
//   - 隧道内层（localMux）：tunnel（隧道加密即认证，裸注册）。
func Routes(eps Endpoints) []router.Route[http.HandlerFunc] {
	return []router.Route[http.HandlerFunc]{
		{Method: "POST", Path: "/summarize", Server: router.ClassNone, Local: router.ClassTunnel, H: eps.Summarize},
		{Method: "POST", Path: "/tag", Server: router.ClassNone, Local: router.ClassTunnel, H: eps.Tag},
		{Method: "GET", Path: "/quota", Server: router.ClassPublic, Local: router.ClassTunnel, H: eps.Quota},
		{Method: "GET", Path: "/privacy", Server: router.ClassPublic, Local: router.ClassTunnel, H: eps.Privacy},
		{Method: "POST", Path: "/privacy/purge", Server: router.ClassPublic, Local: router.ClassTunnel, H: eps.PrivacyPurge},
	}
}
