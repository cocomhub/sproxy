// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package router 是**路由声明的数据化 + 双 mux 注册**内核。
//
// 背景：`pkg/server` 的 `RegisterRoutes` 曾是 613 行单函数、231 处内联注册，同一
// (method, path) 要在 `srvMux`（外部面，SproxySig 验签）与 `localMux`（隧道内层，
// 隧道加密即认证）各写一遍，包装器决定鉴权分类，新增端点必须改装配根。
//
// 本包把「路由」表达为**数据**（`Route[H]`）：路径与方法由**拥有该能力的包**声明，
// 装配层只提供 mux 与「分类 → 包装器」映射并调用 `Register`。于是：
//   - 子包管理自己的 URL 子路径（`Routes()` 里一行一条，前缀 + 相对路径）；
//   - 鉴权分类仍是**装配层**的决定（`Class` → wrapper 由装配层传入，子包不能自选）；
//   - 双 mux 注册一次写完，不会漏掉另一半；
//   - 路由表可被门禁静态解析（字面量表 + 前缀常量），deny-by-default 不依赖运行期。
//
// 门禁 R38（`route_classification_gate_test.go`）解析 `Routes()` 里的字面量条目，
// 与 `route_classes.tsv` 双向比对。
package router

import (
	"fmt"
	"net/http"
	"strings"
)

// Class 是路由鉴权分类（取值与 `route_classes.tsv` 的分类列同源）。
type Class string

const (
	// ClassPublic 公开端点（健康检查、版本、分享分发）。
	ClassPublic Class = "public"
	// ClassAuth 需 SproxySig 验签（凭据 Ring）。
	ClassAuth Class = "auth"
	// ClassUser 用户面（文件读写：文件组门禁 + 用户身份）。
	ClassUser Class = "user"
	// ClassReader 只读用户面（读子组）。
	ClassReader Class = "reader"
	// ClassNode mesh/hub 节点组。
	ClassNode Class = "node"
	// ClassAdmin 管理端点（角色门 admin）。
	ClassAdmin Class = "admin"
	// ClassTunnel 隧道内层（隧道加密即认证；不注册到 srvMux 时用 ClassNone）。
	ClassTunnel Class = "tunnel"
	// ClassNone 表示**不注册到该 mux**（单向路由）。
	ClassNone Class = ""
)

// Valid 报告分类是否是已知取值。
func (c Class) Valid() bool {
	switch c {
	case ClassPublic, ClassAuth, ClassUser, ClassReader, ClassNode, ClassAdmin, ClassTunnel:
		return true
	}
	return false
}

// Route 是一条路由声明：方法、**相对路径**（相对所属前缀）、两端分类，以及供装配层
// 绑定 handler 的键（H 由调用方决定，通常是 handler 枚举或函数类型）。
//
// 之所以是泛型而不是 `http.HandlerFunc`：装配层的 handler 需要装配态（如 ctx→actor），
// 子包不应持有它（P1-c 裁决）。子包只声明「这是哪个操作」，装配层把 H 映射成 handler。
type Route[H any] struct {
	Method string
	Path   string
	Server Class // srvMux 分类；ClassNone = 不注册到 srvMux
	Local  Class // localMux 分类；ClassNone = 不注册到 localMux
	// Mutating 声明该路由是否**改变状态**（集群只读副本的写面门据此拒绝）。
	// 与 Class 正交：Class 回答「谁能访问」，Mutating 回答「是否写」。
	Mutating bool
	// Guard 是**附加入口控制**的记号（如 "credential_login"）：分类决定基础鉴权，
	// Guard 决定额外策略（限流器 / IP 门）。空串 = 无附加策略。
	// 具体映射由装配层唯一提供（未知记号在注册期 panic，fail-closed）。
	Guard string
	H     H
}

// Wrapper 是 (分类, Guard) → 包装器映射（装配层提供）。
// 返回 nil 表示不包装（裸注册）。
type Wrapper func(Class, string) func(http.HandlerFunc) http.HandlerFunc

// Register 把 routes 注册到 srvMux / localMux，返回注册条数。
//
// prefix 是路由组前缀（如 "/api/cloud"）；空串表示**根组**（Path 已是绝对路径）。
// 每条 Route.Path 须为空（= 组根本身，如 `/api/trash`）或以 "/" 开头的相对路径。
// bind 把 Route.H 映射为 handler；返回 nil 表示该路由无 handler（视为装配错误）。
func Register[H any](
	srvMux, localMux *http.ServeMux,
	prefix string,
	routes []Route[H],
	bind func(H) http.HandlerFunc,
	wrap Wrapper,
) int {
	// 空前缀 = 根组（Path 自带全部路径，如文件域横跨 /upload、/api/files…）。
	if prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		panic(fmt.Sprintf("router: 非法组前缀 %q（须为空或以 / 开头且不以 / 结尾）", prefix))
	}
	n := 0
	for _, r := range routes {
		if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
			panic(fmt.Sprintf("router: 非法相对路径 %q（须为空（组根）或以 / 开头）", r.Path))
		}
		handler := bind(r.H)
		if handler == nil {
			panic(fmt.Sprintf("router: %s %s%s 未绑定 handler", r.Method, prefix, r.Path))
		}
		pattern := r.Method + " " + prefix + r.Path
		if r.Server != ClassNone {
			srvMux.HandleFunc(pattern, wrapHandler(wrap, r.Server, r.Guard, handler))
			n++
		}
		if r.Local != ClassNone {
			localMux.HandleFunc(pattern, wrapHandler(wrap, r.Local, r.Guard, handler))
			n++
		}
	}
	return n
}

// wrapHandler 按 (分类, Guard) 取包装器；未登记的分类直接 panic
// （fail-closed：漏登记分类不允许静默裸注册）。
func wrapHandler(wrap Wrapper, class Class, guard string, h http.HandlerFunc) http.HandlerFunc {
	if !class.Valid() {
		panic(fmt.Sprintf("router: 未知分类 %q（未在 Class 枚举内）", class))
	}
	if wrap == nil {
		return h
	}
	fn := wrap(class, guard)
	if fn == nil {
		return h
	}
	return fn(h)
}
