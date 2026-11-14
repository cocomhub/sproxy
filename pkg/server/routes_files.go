// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_files.go 是**文件域路由表**：本域每条路由只在此声明一次，主 mux 与隧道内层
// 两个面由 `router.Register` 分别按 `Server` / `Local` 分类派生包装器——
// 不再在 routes.go 里手工配对两遍（此前 45 条路由 × 2 个 mux = 90 处注册）。
//
// 分类语义（本表即**单一事实源**，运行期经 route_class_index.go 按 r.Pattern 查表；
// 见 docs/archive/2026-11-03-route-class-single-source.md）：
//   - `Server: ClassUser`   ⇒ fileRoute（写子组，requireRole(user)）
//   - `Server: ClassReader` ⇒ fileRouteRead（只读子组，requireRole(reader)）
//   - `Local:  ClassTunnel` ⇒ 隧道内层裸注册（隧道加密即认证；角色门由 localMuxGate 收口）
//
// 门禁 R38 静态解析本文件（RoutePrefix + Route 字面量）并与 route_classes.tsv 双向比对；
// 改分类必须同时改本表与 TSV，diff 可见、无法静默漂移。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// filesRoutePrefix 是文件域路由组的**空前缀**（路径本身已是绝对路径）。
// 文件域横跨 /upload、/download、/api/files、/api/volumes 等多个顶层路径，
// 故整域用空前缀 + 绝对 Path 声明（router 支持空前缀 = 根组）。
const filesRoutePrefix = ""

// fileDomainRoutes 返回文件域的全部路由（绝对路径 + 两端分类 + 方法表达式绑定）。
//
// 用方法表达式（如 (*Handlers).upload）而非字符串键：handler 改名/删除会在编译期
// 立刻失败，不会静默失配。
func fileDomainRoutes() []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)] {
	return []router.Route[func(*Handlers, http.ResponseWriter, *http.Request)]{
		{Method: "POST", Path: "/upload", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).upload},
		{Method: "GET", Path: "/download", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).download},
		{Method: "POST", Path: "/delete", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).delete},
		{Method: "POST", Path: "/rename", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).rename},
		{Method: "GET", Path: "/api/files", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).listFiles},
		{Method: "HEAD", Path: "/api/files/stat", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).stat},
		{Method: "GET", Path: "/api/du", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).duHandler},
		{Method: "POST", Path: "/upload/init", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).uploadInit},
		{Method: "POST", Path: "/upload/chunk", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).uploadChunk},
		{Method: "GET", Path: "/upload/status", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).uploadStatus},
		{Method: "GET", Path: "/upload/sessions", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).uploadSessions},
		{Method: "POST", Path: "/upload/complete", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).uploadComplete},
		{Method: "GET", Path: "/download/chunk", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).downloadChunk},
		{Method: "POST", Path: "/mkdir", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).mkdir},
		{Method: "POST", Path: "/rmdir", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).rmdir},
		{Method: "GET", Path: "/api/files/search", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).searchFiles},
		{Method: "PUT", Path: "/api/files/meta", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).updateFileMeta},
		{Method: "GET", Path: "/api/files/meta", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).getFileMeta},
		{Method: "GET", Path: "/api/search/semantic", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).semanticSearchHandler},
		{Method: "POST", Path: "/api/tags", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).tagsHandler},
		{Method: "POST", Path: "/api/batch/delete", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).batchDelete},
		{Method: "POST", Path: "/api/batch/rename", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).batchRename},
		{Method: "POST", Path: "/api/archive", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).archiveHandler},
		{Method: "GET", Path: "/api/archive-dir", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).archiveDirHandler},
		{Method: "GET", Path: "/api/versions", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).listVersionsHandler},
		{Method: "POST", Path: "/api/versions/restore", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).restoreVersionHandler},
		{Method: "DELETE", Path: "/api/versions", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).deleteVersionHandler},
		{Method: "GET", Path: "/api/volumes", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).listVolumesHandler},
		{Method: "POST", Path: "/api/volumes/move", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).moveVolumeHandler},
		{Method: "POST", Path: "/api/volumes/rebalance", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).rebalanceVolumeHandler},
		{Method: "POST", Path: "/api/volumes/copy", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).copyVolumeHandler},
		{Method: "POST", Path: "/api/volumes/refresh", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).refreshVolumeHandler},
		{Method: "GET", Path: "/api/volumes/export", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).exportVolumeHandler},
		// 真实存储审查（只读；设计 docs/archive/2026-11-04-secretdata-storage-inspection.md）：
		// 正向落点 + 反向解释。仅实现 StorageInspector 的卷可用（其它卷 501）。
		{Method: "GET", Path: "/api/volumes/{name}/trace", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).volumeTraceHandler},
		{Method: "GET", Path: "/api/volumes/{name}/explain", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).volumeExplainHandler},
		{Method: "POST", Path: "/api/volumes/import", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).importVolumeHandler},
		{Method: "POST", Path: "/api/volumes/user", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).createUserVolumeHandler},
		{Method: "GET", Path: "/api/volumes/user", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).listUserVolumesHandler},
		{Method: "GET", Path: "/api/quota", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).userQuotaHandler},
		{Method: "DELETE", Path: "/api/volumes/user", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).deleteUserVolumeHandler},
		{Method: "GET", Path: "/api/backends", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).backendsHandler},
		{Method: "POST", Path: "/api/backends/{type}/presign", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).backendPresignHandler},
		{Method: "POST", Path: "/api/backends/{type}/presign/complete", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).backendPresignCompleteHandler},
		{Method: "POST", Path: "/api/secrets", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).createSecretHandler},
		{Method: "GET", Path: "/api/secrets", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).listSecretsHandler},
		{Method: "GET", Path: "/api/secrets/{name}", Server: router.ClassReader, Local: router.ClassTunnel, H: (*Handlers).exportSecretHandler},
		{Method: "DELETE", Path: "/api/secrets/{name}", Server: router.ClassUser, Local: router.ClassTunnel, H: (*Handlers).deleteSecretHandler},
	}
}
