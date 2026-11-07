// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

// routes.go 声明本包拥有的**分享管理 URL 子路径**（相对前缀），由装配层的
// `router.Register` 一次注册到 srvMux / localMux —— 双 mux 不再各写一遍。
//
// 鉴权分类（Server / Local）是**装配层的决定**：本包只声明「这条路由在两端分属哪个分类」，
// 不提供 wrapper（wrapper 由装配层按分类注入）。
//
// 公网分发端点 `GET /s/{token}` **不属本组**：它是独立前缀（`/s`）、仅主 mux、public 裸注册，
// 留在装配层 `routes.go` 注册。
//
// 门禁 R38 静态解析下面的字面量表与 RoutePrefix，与 route_classes.tsv 双向比对。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// RoutePrefix 是本包路由组前缀（R38 依赖该常量的字面量）。
const RoutePrefix = "/api"

// Endpoints 是本组各操作在装配层的 handler（`*Handlers` 的方法值）。
//
// H 用**命名结构体字段 + 函数类型**而非字符串键：字段名写错在编译期即失败，且
// 子包不持有装配态的 handler（P1-c 裁决：ctx→actor 等装配语义留在装配层）。
type Endpoints struct {
	Create http.HandlerFunc // POST /api/shares
	List   http.HandlerFunc // GET /api/shares
	Revoke http.HandlerFunc // DELETE /api/shares/{token}
}

// Routes 返回本包拥有的全部路由（相对路径 + 两端分类）。
//
// 分类约定（与现状一致，改动须同步 route_classes.tsv）：
//   - 外部面（srvMux）：创建/撤销 = user（写子组 role 门），列表 = reader（只读子组）；
//   - 隧道内层（localMux）：tunnel（隧道加密即认证，裸注册）。
func Routes(eps Endpoints) []router.Route[http.HandlerFunc] {
	return []router.Route[http.HandlerFunc]{
		{Method: "POST", Path: "/shares", Server: router.ClassUser, Local: router.ClassTunnel, H: eps.Create},
		{Method: "GET", Path: "/shares", Server: router.ClassReader, Local: router.ClassTunnel, H: eps.List},
		{Method: "DELETE", Path: "/shares/{token}", Server: router.ClassUser, Local: router.ClassTunnel, H: eps.Revoke},
	}
}
