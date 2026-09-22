// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sftp 提供 SFTP 存储后端客户端：把任意 SFTP 服务适配为 `pkg/sync.FS`
// （7 方法），作为 sproxy 外部卷（V3 通用卷模型，RegisterBackend("sftp") 接入
// 系统盘/用户卷）。与 webdav 后端同构：Extra 读类型特有配置 → 构造客户端 →
// ExternalBackend 包装；装配层（cmd/sproxy/root.go）调用 Register 注册。
package sftp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// sftpExternalBackend 是 SFTP 卷的 ExternalBackend 实现：持有 SFTPFS 同步视图，
// Close 关底层 ssh/sftp 连接（幂等）。
type sftpExternalBackend struct {
	fs *SFTPFS
}

func (b *sftpExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *sftpExternalBackend) Close() error { return b.fs.Close() }

// Ping 实现 registry.HealthProbe（可选扩展）：探测 SFTP 连接可用性。
func (b *sftpExternalBackend) Ping(ctx context.Context) error { return b.fs.Ping(ctx) }

var (
	_ registry.ExternalBackend = (*sftpExternalBackend)(nil)
	_ registry.HealthProbe     = (*sftpExternalBackend)(nil)
)

// newSFTPBackend 按卷描述构造 SFTP 外部后端（V3 可插拔；用户确认放 pkg/volume/sftp）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "url"：SFTP 地址（sftp://user@host:port[/root-path]；必填，端口默认 22）
//   - "private_key"：私钥内容（与 password 二选一；fail-closed 至少一个）
//   - "password"：密码认证（与 private_key 二选一）
//   - "root"：远端根目录（可选；默认用户主目录）
//
// 凭据（fail-closed）：url 必填且 sftp scheme；private_key 或 password 至少一个非空。
func newSFTPBackend(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("sftp backend: 卷 %q 类型 %q 不是外部 sftp 卷", v.Name, v.Type)
	}
	rawURL, _ := v.Extra["url"].(string)
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("sftp backend: 卷 %q 需配置 extra.url（sftp://user@host:port[/root-path]）", v.Name)
	}
	privateKey, _ := v.Extra["private_key"].(string)
	password, _ := v.Extra["password"].(string)
	root, _ := v.Extra["root"].(string)
	if privateKey == "" && password == "" {
		return nil, fmt.Errorf("sftp backend: 卷 %q 需配置认证（private_key 或 password 至少一个；fail-closed：SFTP 无匿名目标）", v.Name)
	}
	fs, err := NewSFTPFS(ClientConfig{
		URL:        rawURL,
		PrivateKey: privateKey,
		Password:   password,
		Root:       root,
	})
	if err != nil {
		return nil, fmt.Errorf("sftp backend: 卷 %q 客户端构造失败: %w", v.Name, err)
	}
	return &sftpExternalBackend{fs: fs}, nil
}

// registerSFTPBackendWithFactory 注册 sftp 后端类型构造器（测试可用唯一类型名注册，
// 避免与生产 "sftp" 重复 panic）。重复注册 → registry panic（编程错误）。
func registerSFTPBackendWithFactory(typ string) {
	registry.RegisterBackend(typ, newSFTPBackend)
}

// RegisterSFTPBackend 注册 sftp 后端（装配层 root.go 调用）。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic。
var registerSFTPOnce sync.Once

func RegisterSFTPBackend() {
	registerSFTPOnce.Do(func() {
		registerSFTPBackendWithFactory("sftp")
	})
}
