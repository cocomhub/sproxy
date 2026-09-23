// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dav.go 是本地卷 WebDAV 服务端挂载面（roadmap P1 服务端 WebDAV）：
//
//   - /dav/ 路由（srvMux，经 authMiddleware 认证）→ davHandler。
//   - 按请求 owner（auth 注入 actor）解析租户 user 桶根 → LocalFS 桥接
//     webdav.NewHandler —— 任意 WebDAV 客户端（curl / 文件管理器 / rsync）
//     直接读写本服务存储（owner 隔离，只能读写自己 user 桶）。
//   - 复用 pkg/gateway/webdav（sync.FS → RFC 4918 适配已完备）。

import (
	"net/http"
	"path/filepath"

	webdavcore "github.com/cocomhub/sproxy/pkg/gateway/webdav/webdavcore"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// davHandler 按请求者 owner 提供 WebDAV 挂载（user 桶根）。
// 未认证 owner（空）→ anonymous 租户（allowInsecureLoopback 已由 authMiddleware 门）。
func (h *Handlers) davHandler(w http.ResponseWriter, r *http.Request) {
	owner := ownerFromRequest(r)
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "卷不存在", http.StatusBadRequest)
		return
	}
	userAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		http.Error(w, "卷根不可用", http.StatusInternalServerError)
		return
	}
	// LocalFS 需要绝对路径；user 桶即 WebDAV 根（路径相对 user 桶）。
	fs := sync.NewLocalFS(filepath.ToSlash(userAbs), h.logger)
	dav := webdavcore.NewHandler(fs)
	if dav == nil {
		http.Error(w, "webdav 不可用", http.StatusInternalServerError)
		return
	}
	dav.ServeHTTP(w, r)
}
