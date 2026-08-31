// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"fmt"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// 阶段 5 工作项 1：服务端 xfer listener 装配辅助。
//
// PR-3 在 cmd/sproxy/root.go 装配第二条 accept 循环（xfer TLS listener，直接对接
// `sclient tunnel --xfer tcp+tls`）时使用。本文件只提供纯装配逻辑（配置提取 +
// TLS 构造 + 密钥派生 + 身份加载），不包含任何监听/路由逻辑。
//
// 关键正确性点（AD-3/AD-4）：
//   - 隧道密钥 = access_keys[0].Secret → tunnel.DeriveTunnelKey(sk, AccessKeyMesh(ak))，
//     与客户端同一派生实现（禁止各写一套 mesh 解析）；
//   - 服务端 Ed25519 身份独立于 TLS 证书：TLS 管传输机密性（复用 cfg.TLS.* 证书，
//     含 auto_tls 自签），Ed25519 管握手指纹 pinning。

// XferListenerConfig 是服务端 xfer TLS listener 的装配配置，从 server.Config 提取。
type XferListenerConfig struct {
	// Enabled 是否启用 xfer listener。当前无专属配置段（避免配置膨胀），由
	// root.go 按 hub.transports.xfer_tcp/xfer_tls 决定后填入；字段保留供未来下移。
	Enabled bool
	// Listen 监听地址（host:port）。空 = 由 root.go 回落默认 loopback
	// （xfer TLS listener 默认绑 loopback，远程可达须显式 listen）。
	Listen string

	// CertFile/KeyFile 是 xfer TLS 证书文件。当前无 xfer 专属证书配置段，
	// 缺省回落 cfg.TLS.CertFile/KeyFile（BuildXferTLSConfig 经 certmgr 统一加载，
	// 含 auto_tls 自签，与主 HTTP listener 复用同一证书）；字段保留供未来独立证书。
	CertFile string
	KeyFile  string

	// AccessKey/AccessKeySecret/MeshID 是隧道密钥派生参数（access_keys 首对）。
	// HubXferKey 的单一输入来源（AD-3：与客户端同 AK/SK 派生一致）。
	AccessKey       string
	AccessKeySecret string
	MeshID          string
}

// XferListenerConfigFromConfig 从 server.Config 提取 xfer listener 装配配置：
//   - CertFile/KeyFile 缺省回落 cfg.TLS.CertFile/KeyFile；
//   - AccessKey/AccessKeySecret/MeshID 取 access_keys 首对。
func XferListenerConfigFromConfig(cfg *Config) XferListenerConfig {
	var xc XferListenerConfig
	if cfg == nil {
		return xc
	}
	xc.CertFile = cfg.TLS.CertFile
	xc.KeyFile = cfg.TLS.KeyFile
	if len(cfg.AccessKeys) > 0 {
		xc.AccessKey = cfg.AccessKeys[0].Key
		xc.AccessKeySecret = cfg.AccessKeys[0].Secret
		xc.MeshID = cfg.AccessKeys[0].MeshID
	}
	return xc
}

// BuildXferTLSConfig 构造服务端 xfer listener 的 *tls.Config。
//
// 证书加载对齐 root.go startTLSListener 的 certmgr 模式：
//   - cert_file/key_file（缺省回落 cfg.TLS.*）→ 文件证书；
//   - cfg.TLS.AutoTLS → 自签证书（与主 HTTP server 复用同一自签证书文件）；
//   - 两者皆无 → error fail-closed（xfer TLS listener 不允许无证书明文承载）。
//
// MinVersion 固定 TLS1.2（对齐仓库其他传输层 TLS 基线）。
func BuildXferTLSConfig(cfg *Config) (*tls.Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("build xfer tls: 配置为 nil")
	}
	xc := XferListenerConfigFromConfig(cfg)
	// 仅复用证书/自签逻辑；xfer 是 raw TLS listener，不做 HTTP mTLS（ClientCA）——
	// 握手侧身份 pinning 由 Ed25519 指纹层负责（AD-4 解耦）。
	mgr, err := certmgr.New(&certmgr.Config{
		CertFile: xc.CertFile,
		KeyFile:  xc.KeyFile,
		AutoTLS:  cfg.TLS.AutoTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("build xfer tls: 创建证书管理器失败: %w", err)
	}
	defer func() {
		if closeErr := mgr.Close(); closeErr != nil {
			// 证书管理器 Close 仅释放资源（自签/文件证书 Close 均为 no-op），
			// 失败不影响返回的 tls.Config，仅记日志级处理由调用方负责。
			_ = closeErr
		}
	}()
	tc, err := mgr.TLSConfig()
	if err != nil {
		return nil, fmt.Errorf("build xfer tls: 获取 TLS 配置失败: %w", err)
	}
	// 兜底：certmgr 各实现已设 TLS1.2，防御性保证（未来实现变更不回归）。
	if tc.MinVersion == 0 {
		tc.MinVersion = tls.VersionTLS12
	}
	return tc, nil
}

// HubXferKey 从 cfg.AccessKeys 首对派生 xfer 隧道密钥（AD-3）。
//
// 派生参数与客户端完全一致：
//
//	DeriveTunnelKey(access_keys[0].Secret, AccessKeyMesh(access_keys[0].Key))
//
// 无 access_keys → error fail-closed（与规格 DoD 4 一致：无 access_keys 时
// xfer listener 拒启；握手无密钥即无法完成 ECDH，拒绝匿名接入）。
func HubXferKey(cfg *Config) ([]byte, error) {
	if cfg == nil || len(cfg.AccessKeys) == 0 {
		return nil, fmt.Errorf("hub xfer key: 未配置 access_keys（xfer listener 需要 access_keys 首对派生隧道密钥；fail-closed）")
	}
	ak := cfg.AccessKeys[0]
	if ak.Key == "" || ak.Secret == "" {
		return nil, fmt.Errorf("hub xfer key: access_keys[0] 缺少 key/secret（key=%q）", ak.Key)
	}
	key, err := tunnel.DeriveTunnelKey(ak.Secret, tunnel.AccessKeyMesh(ak.Key))
	if err != nil {
		return nil, fmt.Errorf("hub xfer key: 派生隧道密钥失败: %w", err)
	}
	return key, nil
}

// xferIdentityRelDir 是服务端身份文件所在子目录（位于 uploads_dir 下）。
const xferIdentityRelDir = "sproxy"

// xferIdentityFileName 是服务端身份文件名。
const xferIdentityFileName = "server-identity.json"

// XferIdentityPath 返回服务端 xfer Ed25519 身份文件路径。
//
// 默认 <uploads-dir>/sproxy/server-identity.json（AD-4：服务端身份与 TLS 证书解耦，
// 用于握手指纹 pinning）。uploads_dir 为空时回落相对路径 sproxy/server-identity.json。
func XferIdentityPath(cfg *Config) string {
	if cfg == nil || cfg.UploadsDir == "" {
		return filepath.Join(xferIdentityRelDir, xferIdentityFileName)
	}
	return filepath.Join(cfg.UploadsDir, xferIdentityRelDir, xferIdentityFileName)
}

// LoadXferIdentity 加载/生成服务端 xfer Ed25519 身份（AD-4）。
//
// 复用 tunnel.LoadOrCreateIdentity：文件不存在自动生成并保存（权限 0600）；
// 文件已存在直接加载；文件损坏 fail-closed 返回错误（不静默重建覆盖用户文件）。
func LoadXferIdentity(cfg *Config) (*tunnel.Identity, error) {
	id, err := tunnel.LoadOrCreateIdentity(XferIdentityPath(cfg))
	if err != nil {
		return nil, fmt.Errorf("load xfer identity: %w", err)
	}
	return id, nil
}
