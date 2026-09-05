// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/accesskey"
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

	// AccessKey/AccessKeySecret/MeshID 是隧道密钥派生参数（凭据 Ring 首个存活条目）。
	// HubXferKey 的单一输入来源（AD-3：与客户端同 AK/SK 派生一致）。
	AccessKey       string
	AccessKeySecret string
	MeshID          string
}

// XferListenerConfigFromRing 从凭据 Ring 提取 xfer listener 装配配置：
// 证书缺省回落 cfg.TLS.CertFile/KeyFile；AccessKey/AccessKeySecret/MeshID
// 取 Ring 首个存活（alive）条目。cfg 为 nil 时仅返回零值（不 panic）。
func XferListenerConfigFromRing(cfg *Config, ring *accesskey.Ring) XferListenerConfig {
	var xc XferListenerConfig
	if cfg != nil {
		xc.CertFile = cfg.TLS.CertFile
		xc.KeyFile = cfg.TLS.KeyFile
	}
	if ak, sk, ok := bestFirstCredential(ring); ok {
		xc.AccessKey = ak
		xc.AccessKeySecret = sk
		xc.MeshID = accesskey.ParseMesh(ak)
	}
	return xc
}

// XferListenerConfigFromConfig 从 server.Config + 凭据 Ring 提取 xfer listener 装配配置。
// 证书缺省回落 cfg.TLS.CertFile/KeyFile；AccessKey/AccessKeySecret/MeshID 取 Ring
// 首个存活条目（取代已移除的 cfg.AccessKeys[0]）。
func XferListenerConfigFromConfig(cfg *Config, ring *accesskey.Ring) XferListenerConfig {
	return XferListenerConfigFromRing(cfg, ring)
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
	xc := XferListenerConfigFromConfig(cfg, nil) // BuildXferTLSConfig 只消费证书，不依赖凭据
	// 仅复用证书/自签逻辑；xfer 是 raw TLS listener，不做 HTTP mTLS（ClientCA）——
	// 握手侧身份 pinning 由 Ed25519 指纹层负责（AD-4 解耦）。
	mgr, err := certmgr.New(&certmgr.Config{
		CertFile: xc.CertFile,
		KeyFile:  xc.KeyFile,
		AutoTLS:  cfg.TLS.AutoTLS,
	})
	if err != nil {
		// 审查 I-2：xfer TLS 不消费 cfg.TLS.ACME（证书是懒加载 GetCertificate + HTTP-01
		// 常驻，与 listener 生命周期耦合，不适合复用到独立 xfer listener）。ACME 部署
		// 下用户需为 xfer 显式配 cert_file/key_file 或开 auto_tls，错误信息明示避免误导。
		if cfg.TLS.ACME.Enabled {
			return nil, fmt.Errorf("build xfer tls: xfer listener 不支持 ACME 证书，请配置 tls.cert_file/tls.key_file 或 tls.auto_tls: true（ACME 与 xfer listener 生命周期不兼容）")
		}
		return nil, fmt.Errorf("build xfer tls: 创建证书管理器失败（需配置 tls.cert_file/tls.key_file 或 tls.auto_tls: true）: %w", err)
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

// HubXferKey 从凭据 Ring 首个存活条目派生 xfer 隧道密钥（AD-3）。
//
// 派生参数与客户端完全一致：
//
//	DeriveTunnelKey(firstAlive.SK, AccessKeyMesh(firstAlive.AK))
//
// Ring 为空（无可存活条目）→ error fail-closed（与规格 DoD 4 一致：无有效凭据时
// xfer listener 拒启）。C-1 修复后此注释成立：ECDH 握手会话密钥派生绑定静态密钥
// （deriveSessionKey），攻击者完成匿名 ECDH 但不知 key，派生出的 sessionKey 与合法
// 对端不同，首个加密帧 AES-GCM 解密失败即被拒——拒绝匿名接入由密钥绑定保证，而非
// 仅靠"无 key 无法完成 ECDH"（后者在旧实现中不成立）。
func HubXferKey(cfg *Config, ring *accesskey.Ring) ([]byte, error) {
	ak, sk, ok := bestFirstCredential(ring)
	if !ok {
		return nil, fmt.Errorf("hub xfer key: 凭据 Ring 为空（xfer listener 需要有效 AK/SK 派生隧道密钥；fail-closed）")
	}
	key, err := tunnel.DeriveTunnelKey(sk, accesskey.ParseMesh(ak))
	if err != nil {
		return nil, fmt.Errorf("hub xfer key: 派生隧道密钥失败: %w", err)
	}
	return key, nil
}

// xferIdentityRelDir 是服务端身份文件所在子目录（位于 XDG 用户配置目录下）。
const xferIdentityRelDir = "sproxy"

// xferIdentityFileName 是服务端身份文件名。
const xferIdentityFileName = "server-identity.json"

// XferIdentityPath 返回服务端 xfer Ed25519 身份文件路径。
//
// 优先级：
//  1. cfg.Hub.XferIdentityFile（显式配置，运维可指定任意受保护目录）；
//  2. 回落 XDG 用户配置目录 os.UserConfigDir()/sproxy/server-identity.json
//     （Linux ~/.config/sproxy/、macOS ~/Library/Application Support/sproxy/、
//     Windows %AppData%/sproxy/，与 sclient XDG 配置约定一致）。
//
// **安全约束（审查 C-1）**：绝不把身份文件放 storage_root 下——该目录与文件 API
// 的用户可控命名空间重叠，已认证 peer 可经 GET /download?filename=sproxy/... 读取
// 私钥、经上传/删除覆盖替换，击穿 AD-4 pinning 信任锚。XDG 用户配置目录默认对
// 本进程用户可写、对 mesh peer 不可经 HTTP 触达。
func XferIdentityPath(cfg *Config) string {
	if cfg != nil && cfg.Hub.XferIdentityFile != "" {
		return cfg.Hub.XferIdentityFile
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		// os.UserConfigDir 在极少数平台/环境下可能报错（如 $HOME 未设置）。
		// 此时回落相对路径（保留文件名语义），仍**不在 storage_root 下**——
		// 相比放 storage_root 的私钥泄露风险，这是可接受的最后兜底。
		return filepath.Join(xferIdentityRelDir, xferIdentityFileName)
	}
	return filepath.Join(base, xferIdentityRelDir, xferIdentityFileName)
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
