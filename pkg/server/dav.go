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
	"os"
	"path/filepath"
	"strings"

	webdavcore "github.com/cocomhub/sproxy/pkg/gateway/webdav/webdavcore"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// davHandler 按请求者 owner 提供 WebDAV 挂载（user 桶根）。
// 未认证 owner（空）→ anonymous 租户（allowInsecureLoopback 已由 authMiddleware 门）。
func (h *Handlers) davHandler(w http.ResponseWriter, r *http.Request) {
	owner := ownerFromRequest(r)
	// 多卷支持（roadmap P1 残余）：?volume=<name> 显式选卷（ACL 校验）；
	// 缺省走默认卷（tenantFor 零回归）。
	var tnt *storage.Tenant
	if vol := r.URL.Query().Get("volume"); vol != "" {
		if h.volSet == nil {
			http.Error(w, "卷集合未装配", http.StatusBadRequest)
			return
		}
		v, ok := h.volSet.ByName(vol)
		if !ok || !v.Authorize(owner) {
			http.Error(w, "卷不可用", http.StatusForbidden)
			return
		}
		tnt = h.volSet.Tenant(vol, owner, h.logger)
	} else {
		tnt = h.tenantFor(owner)
	}
	if tnt == nil || tnt.Root() == nil {
		http.Error(w, "卷不存在", http.StatusBadRequest)
		return
	}
	userAbs, ok := tnt.Root().Abs(tnt.UserRoot())
	if !ok {
		http.Error(w, "卷根不可用", http.StatusInternalServerError)
		return
	}
	// 预建 user 桶（幂等）：WebDAV 根目录需存在（首次 PROPFIND /dav/ 返回 207）。
	if merr := os.MkdirAll(userAbs, 0o755); merr != nil {
		http.Error(w, "创建 user 桶失败", http.StatusInternalServerError)
		return
	}
	// LocalFS 需要绝对路径；user 桶即 WebDAV 根（路径相对 user 桶）。
	fs := sync.NewLocalFS(filepath.ToSlash(userAbs), h.logger)
	dav := webdavcore.NewHandler(fs)
	if dav == nil {
		http.Error(w, "webdav 不可用", http.StatusInternalServerError)
		return
	}
	// strip /dav/ 前缀：webdav.Handler 以 '/' 为根，前缀保留会使根 Stat('/dav/') 404。
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/dav")
	if r2.URL.Path == "" {
		r2.URL.Path = "/"
	}
	dav.ServeHTTP(w, r2)
}
