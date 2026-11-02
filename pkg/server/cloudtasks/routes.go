// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

// routes.go 声明本包拥有的 **URL 子路径**（相对前缀），由装配层的 `router.Register`
// 一次注册到 srvMux / localMux —— 双 mux 不再各写一遍。
//
// 鉴权分类（Server / Local）是**装配层的决定**：本包只声明「这条路由在两端分属哪个分类」，
// 不提供 wrapper（wrapper 由装配层按分类注入）。
//
// 门禁 R38 静态解析下面的字面量表与 RoutePrefix，与 route_classes.tsv 双向比对。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// RoutePrefix 是本包路由组前缀（R38 依赖该常量的字面量）。
const RoutePrefix = "/api/cloud"

// Handler 是「从编排核心取 HTTP 入口」的绑定键。
//
// 用方法表达式（如 (*Service).CreateDownload）而非字符串键：绑定在编译期受检，
// 增删 Service 方法时路由表会立刻编译失败，不会静默失配。
// 装配层再包一层 ActorFrom（ctx→actor 属装配语义，见 P1-c 裁决）。
type Handler func(*Service, http.ResponseWriter, *http.Request, string)

// Routes 返回本包拥有的全部路由（相对路径 + 两端分类）。
//
// 分类约定（与现状一致，改动须同步 route_classes.tsv）：
//   - 外部面（srvMux）：auth（SproxySig 验签）；
//   - 隧道内层（localMux）：tunnel（隧道加密即认证，裸注册）。
func Routes() []router.Route[Handler] {
	return []router.Route[Handler]{
		{Method: "POST", Path: "/download", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).CreateDownload},
		{Method: "POST", Path: "/download/batch", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).CreateBatchDownload},
		{Method: "GET", Path: "/tasks", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ListTasks},
		{Method: "GET", Path: "/tasks/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).GetTask},
		{Method: "POST", Path: "/tasks/{id}/cancel", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).CancelTask},
		{Method: "DELETE", Path: "/tasks/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).DeleteTask},
		{Method: "POST", Path: "/tasks/{id}/resume", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ResumeTask},
		{Method: "POST", Path: "/tasks/{id}/archive", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ArchiveTask},
		{Method: "POST", Path: "/archive", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ArchiveBatch},
		{Method: "POST", Path: "/groups", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).CreateGroup},
		{Method: "GET", Path: "/groups", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ListGroups},
		{Method: "GET", Path: "/groups/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).GetGroup},
		{Method: "POST", Path: "/groups/{id}/cancel", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).CancelGroup},
		{Method: "DELETE", Path: "/groups/{id}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).DeleteGroup},
		{Method: "POST", Path: "/groups/{id}/resume", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ResumeGroup},
		{Method: "POST", Path: "/groups/{id}/archive", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Service).ArchiveGroup},
	}
}
