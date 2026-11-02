// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

// routes.go 声明本包拥有的**checksum 巡检 URL 子路径**（相对前缀），由装配层的
// `router.Register` 一次注册到 srvMux / localMux —— 双 mux 不再各写一遍。
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
const RoutePrefix = "/api"

// Routes 返回本包拥有的全部路由（相对路径 + 两端分类）。
//
// 分类约定（与现状一致，改动须同步 route_classes.tsv）：
//   - 外部面（srvMux）：auth（运维审计面，SproxySig 验签即可；不做文件组门禁）；
//   - 隧道内层（localMux）：tunnel（隧道加密即认证，裸注册）。
func Routes(h http.HandlerFunc) []router.Route[http.HandlerFunc] {
	return []router.Route[http.HandlerFunc]{
		{Method: "POST", Path: "/verify", Server: router.ClassAuth, Local: router.ClassTunnel, H: h},
	}
}
