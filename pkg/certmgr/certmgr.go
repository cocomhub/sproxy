// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package certmgr 提供统一证书生命周期管理接口，支持三种模式：
//
//   - 静态文件证书（CertFile + KeyFile）
//   - ACME 自动证书（ACME.Enabled + Domains）
//   - 自签证书（AutoTLS，默认 fallback）
//
// 用法:
//
//	mgr, err := certmgr.New(&certmgr.Config{
//	    AutoTLS: true,
//	})
//	if err != nil { ... }
//	tc, err := mgr.TLSConfig()
//	srv := &http.Server{Addr: ":443", TLSConfig: tc}
//	srv.ListenAndServeTLS("", "")
package certmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// DNSProvider 抽象 DNS-01 ACME 挑战的 DNS 记录操作。
type DNSProvider interface {
	// SetDNSRecord 设置 DNS TXT 记录用于域名验证。
	// domain 是待验证的域名，token 是 ACME 挑战令牌，keyAuth 是 key authorization。
	SetDNSRecord(ctx context.Context, domain, token, keyAuth string) error

	// CleanupDNSRecord 清理验证记录。
	CleanupDNSRecord(ctx context.Context, domain, token, keyAuth string) error
}

// Manager 统一证书生命周期管理接口。
type Manager interface {
	// TLSConfig 返回适用于 http.Server 的 tls.Config。
	TLSConfig() (*tls.Config, error)

	// Ready 返回证书是否就绪。
	Ready() bool

	// Close 释放资源。
	Close() error
}

// ACMEConfig 是 ACME 自动证书的配置。
type ACMEConfig struct {
	Enabled  bool
	Domains  []string
	Email    string
	CacheDir string // 默认 "certs/acme"
	HTTP01   bool   // 启用 HTTP-01 挑战（需 80 端口）
	// HTTP01Port 指定 HTTP-01 挑战服务器的监听端口。
	// 设置为空时使用默认值 ":80"。
	// 测试时可设为 "127.0.0.1:0" 使用随机端口。
	HTTP01Port string
}

// Config 是证书管理的通用配置。
//
// 优先级说明（从高到低）：
//  1. CertFile + KeyFile：显式指定证书文件路径时，优先使用文件证书管理器
//  2. ACME.Enabled：启用 ACME 自动证书时，使用 autocert 管理器
//  3. AutoTLS：以上均未配置时，自动生成 ECDSA P-256 自签证书
type Config struct {
	// 方式 1：静态文件证书（最高优先级）
	// 显式指定了证书文件路径时，优先使用文件证书，忽略其他配置。
	CertFile string
	KeyFile  string

	// 方式 2：ACME 自动证书
	// 启用 ACME 时需要配置 Domains（域名列表），支持 HTTP-01 和 TLS-ALPN-01 挑战。
	// DNS-01 挑战支持通过 DNSProvider + DNSConfig 扩展（预留，当前版本未集成）。
	ACME ACMEConfig

	// 方式 3：自签证书（默认 fallback）
	// 开发环境或内网服务使用，自动生成到 certs/ 目录。
	AutoTLS bool

	// mTLS 支持
	ClientCA string // CA 证书路径，非空时启用客户端证书验证

	// DNSProvider 是 ACME DNS-01 挑战的 DNS 提供者名称。
	// 预留字段：当前版本未集成到 autocert 流程中，未来可通过注册表实现
	// 插件化 DNS-01 挑战（如 dnspod、cloudflare 等）。
	// 使用方式：DNSProvider: "dnspod", DNSConfig: { "secret_id": "...", "secret_key": "..." }
	DNSProvider string
	DNSConfig   map[string]string
}

// New 根据配置创建对应的 Manager。
//
// 优先级规则（避免歧义）：
//  1. CertFile + KeyFile 同时非空 → 文件证书管理器
//  2. ACME.Enabled == true → ACME 自动证书管理器
//  3. AutoTLS == true → 自签证书管理器
//  4. 以上均不匹配 → 返回错误
//
// 注意：如果同时配置了 CertFile+KeyFile 和 ACME.Enabled，
// 文件证书优先，ACME 配置被忽略。如需切换模式，请清空对应字段。
func New(cfg *Config) (Manager, error) {
	switch {
	case cfg.CertFile != "" && cfg.KeyFile != "":
		// 优先级 1：显式指定了证书文件
		return newFileCertManager(cfg)
	case cfg.ACME.Enabled:
		// 优先级 2：ACME 自动证书（域名校验在工厂函数中执行）
		return newACMEManager(cfg)
	case cfg.AutoTLS:
		// 优先级 3：自签证书（开发/内网环境）
		return newSelfSignedManager(cfg)
	default:
		return nil, fmt.Errorf("no TLS configuration provided: specify cert_file+key_file, acme, or auto_tls")
	}
}

// setupMTLS 从 ClientCA 路径加载 CA 证书，返回 tls.Config 的修改函数。
func setupMTLS(clientCA string) (func(*tls.Config), error) {
	if clientCA == "" {
		return nil, nil
	}
	caCert, err := os.ReadFile(clientCA)
	if err != nil {
		return nil, fmt.Errorf("读取 ClientCA 证书失败: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("ClientCA 证书解析失败（非 PEM 格式）")
	}
	return func(tc *tls.Config) {
		tc.ClientAuth = tls.RequireAndVerifyClientCert
		tc.ClientCAs = caPool
		tc.MinVersion = tls.VersionTLS12
	}, nil
}
