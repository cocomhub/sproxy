// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_ops.go 是**运维/管理面路由表**（审计视图、集群视图、配置、统计、用量、
// mesh 视图、备份、checksum 台账、URL 解析、事件流）。
//
// 这些路由此前在两个 mux 上各写一遍（27 处注册），分类靠人工保持一致；漏改一处就会
// 出现「主 mux 要鉴权、隧道内层不鉴权」的静默不对称（与 #1017/#1018 同源）。
//
// 它们横跨多个顶层路径（/api/audit、/api/cluster、/api/config…），无法共用一个非空
// 前缀，故整域用**空前缀 + 绝对 Path**（与 routes_files.go 同法）。
//
// 分类（R38 与 route_classes.tsv 双向比对）：
//   - `ClassAuth`   ⇒ authMiddleware（SproxySig 验签）
//   - `ClassPublic` ⇒ 裸注册（集群只读视图：nodes/self 是跨节点公开面）
//   - `ClassTunnel` ⇒ 隧道内层裸注册（隧道加密即认证）

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// opsRoutePrefix 是运维域路由组的空前缀（Path 自带绝对路径）。
const opsRoutePrefix = ""

// opsRoutes 返回运维/管理面的全部路由。
func opsRoutes() []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)] {
	return []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]{
		// 审计查看面（/api/audit 与导出/冷段区间读）。
		{Method: "GET", Path: "/api/audit", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).auditHandler},
		{Method: "GET", Path: "/api/audit/export", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).auditExportHandler},
		{Method: "GET", Path: "/api/audit/cold/seg", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).auditColdSegHandler},
		// 跨节点只读视图：主 mux 公开（跨节点互访），隧道内层裸注册。
		{Method: "GET", Path: "/api/cluster/nodes", Server: router.ClassPublic, Local: router.ClassTunnel, H: (*Handlers).handleClusterNodes},
		{Method: "GET", Path: "/api/cluster/self", Server: router.ClassPublic, Local: router.ClassTunnel, H: (*Handlers).handleClusterSelf},
		// 运行时配置（GET 读 / PUT 改；SIGHUP 只覆盖软配置）。
		{Method: "GET", Path: "/api/config", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).configHandler},
		{Method: "PUT", Path: "/api/config", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).updateConfigHandler, Mutating: true},
		// mesh 只读视图（owner 过滤即边界，不挂 fileRoute 角色门禁）。
		{Method: "GET", Path: "/api/mesh/status", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).meshStatusHandler},
		{Method: "GET", Path: "/api/mesh/acl", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).meshACLHandler},
		// 统计 / 用量 / 备份 / checksum 台账 / URL 解析 / 事件流。
		{Method: "GET", Path: "/api/stats", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).statsHandler},
		{Method: "GET", Path: "/api/usage/report", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).usageReportHandler},
		{Method: "POST", Path: "/api/backup", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).backupHandler, Mutating: true},
		{Method: "POST", Path: "/api/checksums/rebuild", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).rebuildChecksumsHandler, Mutating: true},
		{Method: "GET", Path: "/api/resolve", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).resolveURL},
		{Method: "GET", Path: "/api/events", Server: router.ClassAuth, Local: router.ClassTunnel, H: (*Handlers).eventsHandler},
	}
}
