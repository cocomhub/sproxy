// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// route_class_index.go 把「路由分类」从**运行时字符串推导**改为**注册期声明的查表**。
//
// 背景（两起线上故障）：分类曾由 `isFileGroupedRoute` / `isReadOnlyFileRoute` 两个
// 字符串白名单在**请求时**重新推导，而白名单里写的是**模式串**（`/api/backends/{type}/presign`），
// 运行时拿到的是**具体路径**（`/api/backends/local/presign`）——
//   - 主 mux：fileRoute 的接线自检 ⇒ **每次 500**；
//   - 隧道内层：判为「非文件组」⇒ **完全不做角色检查**（静默越权）。
// 同一表达法还让 `GET /api/search/semantic` 两面策略不一致（主 mux 500 / 隧道 reader 403）。
//
// 现在的单一事实源是 `fileDomainRoutes()`（`routes_files.go`）：
// 分类在**表里声明一次**，索引键 = ServeMux 注册的模式串，运行期用 `r.Pattern`
// （Go 1.23+ 由 ServeMux 填充的「已匹配模式」）**O(1) 精确查表**——模式串与具体路径
// 的歧义从表示法上消失。
//
// 不可变性：索引由静态表在**包初始化时**构建，运行期只读；不提供注入入口
// （避免测试 seam 让 fail-closed 失真）。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/ai"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/server/notify"
	"github.com/cocomhub/sproxy/pkg/server/router"
	"github.com/cocomhub/sproxy/pkg/server/share"
	"github.com/cocomhub/sproxy/pkg/server/verify"
)

// fileDomainClassIndex 是文件域「模式串 → 分类」索引（键为 "METHOD /path"）。
//
// 来源是**所有声明了文件域分类的表**：装配层 `fileDomainRoutes()` + 子包
// `share.Routes()`（分享是文件组：创建/撤销 ClassUser、列表 ClassReader）。
// 子包表的分类同样是声明式单一事实源，这里只是把它们编入同一个索引。
var fileDomainClassIndex = buildFileDomainClassIndex()

func buildFileDomainClassIndex() map[string]router.Class {
	idx := buildRouteClassIndex(fileDomainRoutes())
	// share.Endpoints 零值仅用于**读取分类**（Routes 是纯函数，不解引用 handler）。
	for _, rt := range share.Routes(share.Endpoints{}) {
		idx[rt.Method+" "+share.RoutePrefix+rt.Path] = rt.Server
	}
	return idx
}

// buildRouteClassIndex 把声明表编译成索引（键与 ServeMux 的 `r.Pattern` 同构）。
func buildRouteClassIndex(routes []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]) map[string]router.Class {
	idx := make(map[string]router.Class, len(routes))
	for _, rt := range routes {
		idx[rt.Method+" "+rt.Path] = rt.Server
	}
	return idx
}

// fileDomainClass 返回该**已匹配模式**在文件域的声明分类。
// 第二返回值为 false 表示「不在文件域表内」（= 既有语义的「非文件组」）。
func fileDomainClass(pattern string) (router.Class, bool) {
	c, ok := fileDomainClassIndex[pattern]
	return c, ok
}

// 写面（mutating）索引：声明式取代 `isWriteFaceRoute` 的路径清单。
//
// 为什么：写面清单曾是**第二份**需要人工维护的分类（10 条规则 + 前缀判断），漏改的后果是
// 集群只读副本上仍可写入（副本与主分叉）。与分类查表同源化后，每条路由的「是否写」由
// 其声明表回答，中间件只做一次查表。
//
// 文件域与分享域由 Class 派生（ClassUser = 写子组）；其余域的写路由显式声明 Mutating。
var mutatingPatterns = buildMutatingIndex()

func buildMutatingIndex() map[string]bool {
	idx := map[string]bool{}
	add := func(method, pattern string, mut bool) { idx[method+" "+pattern] = mut }

	for _, rt := range fileDomainRoutes() {
		add(rt.Method, rt.Path, rt.Server == router.ClassUser)
	}
	for _, rt := range share.Routes(share.Endpoints{}) {
		add(rt.Method, share.RoutePrefix+rt.Path, rt.Server == router.ClassUser)
	}
	for _, rt := range cloudtasks.Routes() {
		add(rt.Method, cloudtasks.RoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range notify.Routes(notify.Endpoints{}) {
		add(rt.Method, notify.RoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range ai.Routes(ai.Endpoints{}) {
		add(rt.Method, ai.RoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range verify.Routes(nil) {
		add(rt.Method, verify.RoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range syncRoutes() {
		add(rt.Method, syncRoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range trashRoutes() {
		add(rt.Method, trashRoutePrefix+rt.Path, rt.Mutating)
	}
	for _, rt := range opsRoutes() {
		add(rt.Method, rt.Path, rt.Mutating)
	}
	for _, rt := range credentialRoutes() {
		add(rt.Method, credentialRoutePrefix+rt.Path, rt.Mutating)
	}
	return idx
}

// declaredMutating 返回该**已匹配模式**是否声明为写面；ok=false 表示不在任何声明表内。
func declaredMutating(pattern string) (mut bool, ok bool) {
	mut, ok = mutatingPatterns[pattern]
	return mut, ok
}
