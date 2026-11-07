// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_trash.go 是**回收站 API 路由表**：3 条路由只声明一次，两个 mux 由
// `router.Register` 按分类派生包装器（此前两个 mux 各写一遍）。
//
// 分类（R38 与 route_classes.tsv 双向比对）：Server=auth（SproxySig 验签）、
// Local=tunnel（隧道加密即认证）。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// trashRoutePrefix 是本域路由组前缀（R38 依赖该常量的字面量）。
const trashRoutePrefix = "/api/trash"

// trashRoutes 返回回收站域的全部路由。
func trashRoutes() []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)] {
	return []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]{
		{Method: "GET", Path: "", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).listTrashHandler},
		{Method: "POST", Path: "/restore", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).restoreTrashHandler, Mutating: true},
		{Method: "POST", Path: "/empty", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).emptyTrashHandler, Mutating: true},
	}
}
