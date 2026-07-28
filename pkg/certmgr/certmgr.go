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
//	http.ListenAndServeTLS(":443", tc)
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
}

// Config 是证书管理的通用配置。
type Config struct {
	// 方式 1：静态文件证书
	CertFile string
	KeyFile  string

	// 方式 2：ACME 自动证书
	ACME ACMEConfig

	// 方式 3：自签证书（默认 fallback）
	AutoTLS bool

	// mTLS 支持
	ClientCA string // CA 证书路径，非空时启用客户端证书验证

	// DNS Provider 插件名称（如 "dnspod"）
	DNSProvider string
	DNSConfig   map[string]string
}

// New 根据配置创建对应的 Manager。
func New(cfg *Config) (Manager, error) {
	switch {
	case cfg.ACME.Enabled:
		if len(cfg.ACME.Domains) == 0 {
			return nil, fmt.Errorf("acme.enabled 为 true 但 acme.domains 为空")
		}
		return newACMEManager(cfg)
	case cfg.AutoTLS:
		return newSelfSignedManager(cfg)
	case cfg.CertFile != "" && cfg.KeyFile != "":
		return newFileCertManager(cfg)
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
