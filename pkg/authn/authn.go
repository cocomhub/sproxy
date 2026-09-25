// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package authn 是认证面插件化的**共享契约**（DEC-C 提取，2026-09-25 OIDC/LDAP）：
// Authenticator 接口与 Principal 身份模型从 pkg/server 下沉到 G0 基础包，使**独立
// go module 的 ext 认证器**（ext/oidcldap）可以在不导入装配层（pkg/server，R4 禁止）
// 的前提下实现同一契约——pkg/server 以类型别名引用本包，对外 API 与既有调用点
// 零改动。
//
// 归属：纯标准库 + 契约类型，不依赖任何 pkg/* 包（G0 叶子）。
package authn

import (
	"context"
	"net/http"
)

// Principal 是认证成功后的调用方身份（认证面插件化的输出，DEC-C）。
//
// 字段语义（自 pkg/server 迁移，逐字段保持）：
//   - AK：身份锚 + 文件桶 ID——RingAuthenticator 时 = AccessKey（按 AK 落桶现状）；
//     宿主注入者把目标桶 ID 放入本字段（文件操作按 AK 落桶，无需关心 SK 来源）。
//   - Owner：元数据/审计（宿主可映射自有用户名；不参与落桶）。
//   - Role：账号级角色（user|node|admin；空值由 requireRole 归一 user）。
//   - Mesh：该身份所属 mesh（RingAuthenticator 由 AK 派生；宿主可自行填充）。
//   - Secret：明文 SK（R5-I1）——RingAuthenticator 验签成功填充（供 /tunnel 密钥
//     派生）；宿主注入的 Authenticator 留空——无 SproxySig 凭据不派生隧道密钥。
//     SK 仅请求内传递、不落日志、不持久化。
//   - EntryID：RingAuthenticator 验签命中的 SK 条目 ID（供 renew 作 wrap key 亲缘
//     性；宿主注入者留空）。
type Principal struct {
	AK      string
	Owner   string
	Role    string
	Mesh    string
	Secret  []byte
	EntryID string
}

// Authenticator 是认证面插件化接口：把一个 HTTP 请求认证为 Principal（DEC-C）。
// authMiddleware 遍历链：任一成员成功 → 返回的 Principal 入 ctx 并放行；全部失败 →
// 401（或 ring 空时走 allow_insecure_loopback 兜底）。
type Authenticator interface {
	// Name 返回认证器名称（日志 / 诊断用）。
	Name() string
	// Authenticate 校验请求认证。成功返回非 nil Principal；失败返回 error——
	// **不得写响应**（R4-I3：链中失败由后续成员或 authMiddleware 统一处理，
	// 前置写 401 会短路后续 authenticator）。
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// ExternalAuthRoute 是外部认证（OIDC/LDAP）登录面的一条路由（方法 + 路径 + handler）。
// 装配层（pkg/server RegisterRoutes）把外部认证器声明的全部路由同时注册到主 mux 与
// 隧道内层 localMux（浏览器隧道模式下登录页可达）。
type ExternalAuthRoute struct {
	Method  string // HTTP 方法（如 "GET"）；与 Go 1.22 ServeMux pattern 一致
	Pattern string // 路径（如 "/auth/oidc/login"）
	Handler http.Handler
}

// ExternalAuthHandler 是外部认证器登录面的宿主嵌入契约（stdlib-only，供独立 module
// 的 ext 认证器**结构性实现**，无需 import 本包以外的仓内装配层）：
//
//	Name  → 提供者名（日志/诊断）；
//	Routes → 声明登录/回调端点（主 mux + localMux 双注册）；
//	Close → 释放提供者资源（幂等；nil 安全由实现保证）。
type ExternalAuthHandler interface {
	Name() string
	Routes() []ExternalAuthRoute
	Close() error
}
