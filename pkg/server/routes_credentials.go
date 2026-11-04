// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_credentials.go 是**凭据 API 路由表**：10 条路由只声明一次，两个 mux 由
// `router.Register` 按分类 + Guard 派生包装器（此前两个 mux 各写一遍 = 20 处注册）。
//
// 三类安全策略在这里被**显式声明**，而不是散落在注册代码里：
//   - 公开入口（register/nonce/login）：`Server: ClassPublic` + 各自的 `Guard`；
//     Guard 由装配层映射为「限流器 [+ 主 mux 的 IP 门]」——分类决定基础鉴权，
//     Guard 决定入口控制，两者都在装配层唯一定义（`routeWrapper`）。
//   - 管理面（列表/新增/删除/renew/SK）：`Server: ClassAuth`（SproxySig 验签）。
//   - 隧道内层：全部 `Local: ClassTunnel`（隧道加密即认证）。
//
// 历史教训：三条公开入口曾在 localMux 注册时引用**尚未初始化**的限流器字段，
// 方法值捕获 nil 指针 ⇒ 隧道模式下请求直接 panic 断连（客户端只见 EOF）。
// 表化把「限流器初始化 → 注册」的顺序固定下来（`routeWrapper` 对未初始化限流器
// fail-closed panic）。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// credentialRoutePrefix 是本域路由组前缀（R38 依赖该常量的字面量）。
const credentialRoutePrefix = "/api/credentials"

// Guard 记号：公开入口的附加入口控制（限流器 + 主 mux IP 门）。
const (
	guardCredentialRegister = "credential_register"
	guardCredentialNonce    = "credential_nonce"
	guardCredentialLogin    = "credential_login"
)

// credentialRoutes 返回凭据域的全部路由。
func credentialRoutes() []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)] {
	return []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]{
		// 公开入口（免认证；浏览器隧道模式下登录页需可达）。
		{Method: "POST", Path: "/register", Server: router.ClassPublic, Local: router.ClassTunnel, Guard: guardCredentialRegister, H: (*Handlers).registerCredentialHandler},
		{Method: "POST", Path: "/nonce", Server: router.ClassPublic, Local: router.ClassTunnel, Guard: guardCredentialNonce, H: (*Handlers).nonceHandler},
		{Method: "POST", Path: "/login", Server: router.ClassPublic, Local: router.ClassTunnel, Guard: guardCredentialLogin, H: (*Handlers).loginCredentialHandler},
		// 管理面（SproxySig 验签）。
		{Method: "GET", Path: "", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).akListHandler},
		{Method: "POST", Path: "", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).akAddHandler, Mutating: true},
		{Method: "DELETE", Path: "/{ak}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).akDeleteHandler, Mutating: true},
		{Method: "POST", Path: "/{ak}/renew", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).renewCredentialHandler, Mutating: true},
		{Method: "GET", Path: "/{ak}/sk", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).skListHandler},
		{Method: "DELETE", Path: "/{ak}/sk/{skID}", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).skDeleteHandler, Mutating: true},
		{Method: "POST", Path: "/{ak}/sk/{skID}/expire", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).skExpireHandler, Mutating: true},
	}
}
