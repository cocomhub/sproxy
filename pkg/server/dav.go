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
	"strings"

	webdavcore "github.com/cocomhub/sproxy/pkg/gateway/webdav/webdavcore"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// isWebDAVWriteMethod 判定 WebDAV 写方法（数据/目录变更面；读方法 GET/HEAD/PROPFIND/OPTIONS
// 不拦）。占用写保护预检只对写方法生效。
func isWebDAVWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodDelete, "MKCOL", "MOVE", "COPY":
		return true
	}
	return false
}

// davHandler 按请求者 owner 提供 WebDAV 挂载（user 桶根）。
// 未认证 owner（空）→ anonymous 租户（allowInsecureLoopback 已由 authMiddleware 门）。
func (h *Handlers) davHandler(w http.ResponseWriter, r *http.Request) {
	owner := ownerFromRequest(r)
	// 多卷支持（roadmap P1 残余）：?volume=<name> 显式选卷（ACL 校验）；
	// 缺省走默认卷（tenantFor 零回归）。
	volName := r.URL.Query().Get("volume")
	var tnt *storage.Tenant
	if vol := volName; vol != "" {
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
		// 默认卷 ACL 与文件 API 面同口径（评审 P2：此前 `/download`、`/api/files` 等经
		// defaultVolumeAllows 收敛，而 DAV 仍可读写同一 user 桶 ⇒ 收紧默认卷视图后仍能绕过）。
		if !h.defaultVolumeAllows(owner) {
			http.Error(w, "卷不可用", http.StatusForbidden)
			return
		}
		tnt = h.tenantFor(owner)
		volName = h.defaultVolumeName()
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
	if os.MkdirAll(userAbs, 0o755) != nil {
		http.Error(w, "创建 user 桶失败", http.StatusInternalServerError)
		return
	}
	// **经 Root 适配器**（评审 P0 结构性修复）：DAV 此前用 sync.NewLocalFS(<绝对路径>) 纯 os I/O
	// ⇒ ① 绕过 at-rest 加解密（读密文/写明文）；② 绕过封装封印（被封源目录仍可 DAV 直连）。
	// PrefixFS 把 DAV 相对路径映射到 user 桶（同时负责可选能力转发，R21）。
	// davMetaFS 再包一层：写/删/改名联动 sidecar（防裸覆盖写留陈旧 meta 固化失败）。
	fs := &davMetaFS{
		FS:    sync.NewPrefixFS(NewRootFS(tnt.Root()), tnt.UserRoot()+"/"),
		h:     h,
		owner: owner,
		root:  tnt.Root(),
	}
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
	// 写保护（用户语义 #6，旁路闭环 2026-10-06）：WebDAV 写方法目标命中被封装卷占用的子目录
	// → 直接 403（webdavcore 把写面错误映射为 405，预检给明确 403；FS 装饰器作纵深防御）。
	// 读方法（GET/HEAD/PROPFIND/OPTIONS）不拦。
	if h.davWriteGuard(w, r2, volName) {
		return
	}
	dav.ServeHTTP(w, r2)
}

// davWriteGuard 对 WebDAV 写方法做占用写保护预检：命中 → 写 403 并返回 true（已处理）；
// 读方法/未命中/路径为空 → false（放行）。
func (h *Handlers) davWriteGuard(w http.ResponseWriter, r *http.Request, volName string) bool {
	if !isWebDAVWriteMethod(r.Method) {
		return false
	}
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" || rel == "." {
		return false
	}
	if err := h.checkWrapperOccupiedWrite(volName, rel); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return true
	}
	// I2（2026-10-06）：MOVE/COPY 源侧——Destination 头是源路径，命中占用子目录 → 403
	// （「move 搬走 = 删源」同「只读」语义；防把封装卷密文经 MOVE/COPY 搬出破坏）。
	if r.Method == "MOVE" || r.Method == "COPY" {
		if src := davDestinationRel(r); src != "" {
			if err := h.checkWrapperOccupiedWrite(volName, src); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return true
			}
		}
	}
	return false
}

// davDestinationRel 从 MOVE/COPY 的 Destination 头提取卷内相对路径（去 /dav 前缀；
// 无法解析 → ""，调用方按不拦截处理——davWriteGuard 的目标侧检查已兜底）。
func davDestinationRel(r *http.Request) string {
	dest := r.Header.Get("Destination")
	if dest == "" {
		return ""
	}
	// 兼容绝对 URI（http://host/dav/...）与相对（/dav/...）两种形态。
	if i := strings.Index(dest, "/dav"); i >= 0 {
		dest = dest[i:]
	}
	u := strings.TrimPrefix(dest, "/dav")
	u = strings.TrimPrefix(u, "/")
	if u == "" || u == "." {
		return ""
	}
	return u
}
