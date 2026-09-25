// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ftp 提供 FTP 存储后端客户端：把任意 FTP 服务适配为 `pkg/sync.FS`
// （7 方法），作为 sproxy 外部卷（V3 通用卷模型，RegisterBackend("ftp") 接入
// 系统盘/用户卷）。与 sftp/webdav 后端同构：Extra 读类型特有配置 → 构造客户端 →
// ExternalBackend 包装；装配层（cmd/sproxy/root.go）调用 Register 注册。
package ftp

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

// ftpExternalBackend 是 FTP 卷的 ExternalBackend 实现：持有 FTPFS 同步视图，
// Close 关底层控制连接（幂等）。
type ftpExternalBackend struct {
	fs *FTPFS
}

func (b *ftpExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *ftpExternalBackend) Close() error { return b.fs.Close() }

// Ping 实现 registry.HealthProbe（可选扩展）：探测 FTP 连接可用性。
func (b *ftpExternalBackend) Ping(ctx context.Context) error { return b.fs.Ping(ctx) }

var (
	_ registry.ExternalBackend = (*ftpExternalBackend)(nil)
	_ registry.HealthProbe     = (*ftpExternalBackend)(nil)
)

// newFTPBackend 按卷描述构造 FTP 外部后端（V3 可插拔）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "url"：FTP 地址（ftp://user@host:port[/root-path]；必填，端口默认 21）
//   - "password"：密码认证（必填；fail-closed：FTP 无匿名目标）
//   - "root"：远端根目录（可选；默认服务器登录目录）
//
// 凭据（fail-closed）：url 必填且 ftp scheme；password 非空。
func newFTPBackend(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("ftp backend: 卷 %q 类型 %q 不是外部 ftp 卷", v.Name, v.Type)
	}
	rawURL, _ := v.Extra["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("ftp backend: 卷 %q 需配置 extra.url（ftp://user@host:port[/root-path]）", v.Name)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "ftp" || u.User == nil || u.User.Username() == "" || u.Host == "" {
		return nil, fmt.Errorf("ftp backend: 卷 %q extra.url 非法（应为 ftp://user@host[:port][/path]）: %q", v.Name, rawURL)
	}
	password, _ := v.Extra["password"].(string)
	if password == "" {
		return nil, fmt.Errorf("ftp backend: 卷 %q 需配置认证（extra.password；fail-closed：FTP 无匿名目标）", v.Name)
	}
	root, _ := v.Extra["root"].(string)
	fs, err := NewFTPFS(ClientConfig{
		URL:      rawURL,
		Password: password,
		Root:     root,
	})
	if err != nil {
		return nil, fmt.Errorf("ftp backend: 卷 %q 客户端构造失败: %w", v.Name, err)
	}
	return &ftpExternalBackend{fs: fs}, nil
}

// registerFTPBackendWithFactory 注册 ftp 后端类型构造器（测试可用唯一类型名注册，
// 避免与生产 "ftp" 重复 panic）。重复注册 → registry panic（编程错误）。
func registerFTPBackendWithFactory(typ string) {
	registry.RegisterBackend(typ, newFTPBackend)
}

// RegisterFTPBackend 注册 ftp 后端（装配层 root.go 调用）。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic。
var registerFTPOnce sync.Once

func RegisterFTPBackend() {
	registerFTPOnce.Do(func() {
		registerFTPBackendWithFactory("ftp")
	})
}
