// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_sync.go 是**文件同步 API 路由表**：本域每条路由只声明一次，主 mux 与隧道内层
// 两个面由 `router.Register` 分别按 `Server` / `Local` 分类派生包装器。
//
// 此前 9 条路由在两个 mux 上各写一遍（18 处注册），分类靠人工保持一致；漏改一处就会出现
// 「主 mux 要鉴权、隧道内层不鉴权」这类静默不对称（与 #1017/#1018 同源缺陷）。
//
// 分类（与 route_classes.tsv 双向比对，R38）：
//   - Server: ClassAuth   ⇒ authMiddleware（SproxySig 验签）
//   - Local:  ClassTunnel ⇒ 裸注册（隧道加密即认证）
//
// 注：同步任务在 syncMgr 未装配时由 handler 返回 400，与分类无关。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// syncRoutePrefix 是本域路由组前缀（R38 依赖该常量的字面量）。
const syncRoutePrefix = "/api/sync"

// syncRoutes 返回文件同步域的全部路由（相对路径 + 两端分类 + 方法表达式绑定）。
func syncRoutes() []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)] {
	return []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]{
		{Method: "POST", Path: "/tasks", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncCreateTask},
		{Method: "GET", Path: "/tasks", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncListTasks},
		{Method: "GET", Path: "/tasks/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncGetTask},
		{Method: "POST", Path: "/tasks/{id}/cancel", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncCancelTask},
		{Method: "POST", Path: "/tasks/{id}/retry", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncRetryTask},
		{Method: "DELETE", Path: "/tasks/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncDeleteTask},
		{Method: "GET", Path: "/conflicts", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncListConflicts},
		{Method: "GET", Path: "/conflicts/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncGetConflict},
		{Method: "POST", Path: "/conflicts/{id}/resolve", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).syncResolveConflict},
	}
}
