// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dav

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
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// bindVolumeForAudit 把「本次 DAV 请求作用到的卷」绑定进 ctx（审计第 6 维，评审 R13）：
// 显式 `?volume=` 优先；缺省取默认卷（DAV 无参时按构造走默认卷）。空名不写（withAuditVolume 语义）。
// 抽成函数是为了不给 davHandler 增加 gocognit（门禁阈值 15）。
func (s *Service) bindVolumeForAudit(r *http.Request, name string) *http.Request {
	if name == "" && s.deps.Volumes != nil {
		name = s.deps.Volumes.Default().Name
	}
	return r.WithContext(s.deps.WithAuditVolume(r.Context(), name))
}

// davHandler 按请求者 owner 提供 WebDAV 挂载（user 桶根）。
// 未认证 owner（空）→ anonymous 租户（allowInsecureLoopback 已由 authMiddleware 门）。
func (s *Service) Handler(w http.ResponseWriter, r *http.Request) {
	owner := s.deps.OwnerFromRequest(r)
	// 多卷支持（roadmap P1 残余）：?volume=<name> 显式选卷（ACL 校验）；
	// 缺省走默认卷（tenantFor 零回归）。
	volName := r.URL.Query().Get("volume")
	// 审计第 6 维（评审 R13）：显式卷或默认卷都绑定 ⇒ DAV 写操作可归属到卷。
	r = s.bindVolumeForAudit(r, volName)
	var tnt *storage.Tenant
	if vol := volName; vol != "" {
		if s.deps.Volumes == nil {
			http.Error(w, "卷集合未装配", http.StatusBadRequest)
			return
		}
		v, ok := s.deps.Volumes.ByName(vol)
		if !ok || !v.Authorize(owner) {
			http.Error(w, "卷不可用", http.StatusForbidden)
			return
		}
		tnt = s.deps.Volumes.Tenant(vol, owner, s.deps.Logger)
	} else {
		// 默认卷 ACL 与文件 API 面同口径（评审 P2：此前 `/download`、`/api/files` 等经
		// defaultVolumeAllows 收敛，而 DAV 仍可读写同一 user 桶 ⇒ 收紧默认卷视图后仍能绕过）。
		if !s.deps.DefaultVolumeAllows(owner) {
			http.Error(w, "卷不可用", http.StatusForbidden)
			return
		}
		tnt = s.deps.TenantFor(owner)
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
		FS:    sync.NewPrefixFS(rootfs.New(tnt.Root()), tnt.UserRoot()+"/"),
		deps:  s.deps,
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
	dav.ServeHTTP(w, r2)
}
