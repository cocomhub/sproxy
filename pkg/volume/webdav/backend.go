// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// backend.go 是 WebDAV 存储后端的 V3 plugin 接入：把 WebDAVFS（sync.FS 客户端）包装为
// registry.ExternalBackend，经 RegisterBackend("webdav") 注册——系统盘（volumes[]
// type=webdav）与用户卷（POST /api/volumes/user type=webdav）统一走 V3 装配分派。
//
// 与 baidupcs backend（cmd/sproxy/baidupcs_sync.go）同构：Extra 读类型特有配置 → 构造
// 客户端 → ExternalBackend 包装；装配层（cmd/sproxy/root.go）调用 Register 注册。
package webdav

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// webdavExternalBackend 是 WebDAV 卷的 ExternalBackend 实现：持有 WebDAVFS 同步视图，
// Close 关底层 HTTP 空闲连接（幂等）。
type webdavExternalBackend struct {
	fs *WebDAVFS
}

func (b *webdavExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *webdavExternalBackend) Close() error { return b.fs.Close() }

var _ registry.ExternalBackend = (*webdavExternalBackend)(nil)

// newWebDAVBackend 按卷描述构造 WebDAV 外部后端（V3 可插拔；用户确认放 pkg/volume/webdav）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "url"：WebDAV 根 URL（必填，http(s)://host[:port][/webdav-root]）；
//   - "username"/"password"：Basic 认证（两者非空时启用）；
//   - "token"：Bearer 认证（非空时优先于 Basic）；
//   - "local_root"：本地中间态基目录（预留：与 baidupcs 一致的中间态约束；
//     当前 WebDAVFS 无 staging——WriteFile 直接 PUT，OpenRead GET 直连）。
//
// 凭据（fail-closed）：url 必填且 http(s) scheme；username/password 或 token 至少一个
// 非空——否则无认证访问 WebDAV（多数服务要求认证，无匿名目标）。
func newWebDAVBackend(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("webdav backend: 卷 %q 类型 %q 不是外部 webdav 卷", v.Name, v.Type)
	}
	rawURL, _ := v.Extra["url"].(string)
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("webdav backend: 卷 %q 需配置 extra.url（WebDAV 根 URL，http(s)://host[:port][/webdav-root]）", v.Name)
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("webdav backend: 卷 %q extra.url 非法（应为 http(s)://host[:port]）: %q", v.Name, rawURL)
	}
	username, _ := v.Extra["username"].(string)
	password, _ := v.Extra["password"].(string)
	token, _ := v.Extra["token"].(string)
	if token == "" && (username == "" || password == "") {
		return nil, fmt.Errorf("webdav backend: 卷 %q 需配置认证（username+password 或 token 至少一组；fail-closed：WebDAV 无匿名目标）", v.Name)
	}
	// local_root 预留（当前 WebDAVFS 无 staging；保留字段供未来中间态扩展——与
	// baidupcs 一致的中间态约束：staging/cache/tmp 落本地）。
	// _ = v.Extra["local_root"]

	fs, err := NewWebDAVFS(ClientConfig{
		RootURL:  rawURL,
		Username: username,
		Password: password,
		Token:    token,
	})
	if err != nil {
		return nil, fmt.Errorf("webdav backend: 卷 %q 客户端构造失败: %w", v.Name, err)
	}
	return &webdavExternalBackend{fs: fs}, nil
}

// registerWebDAVBackendWithFactory 注册 webdav 后端类型构造器（测试可用唯一类型名注册，
// 避免与生产 "webdav" 重复 panic）。重复注册 → registry panic（编程错误）。
func registerWebDAVBackendWithFactory(typ string) {
	registry.RegisterBackend(typ, newWebDAVBackend)
}

// RegisterWebDAVBackend 注册 webdav 后端（装配层 root.go 调用）。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic。
var registerWebDAVOnce sync.Once

func RegisterWebDAVBackend() {
	registerWebDAVOnce.Do(func() {
		registerWebDAVBackendWithFactory("webdav")
	})
}
