// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certmgr

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"

	"golang.org/x/crypto/acme/autocert"
)

// acmeManager 管理 ACME 自动证书。
type acmeManager struct {
	m      *autocert.Manager
	mTLSFn func(*tls.Config)
	http01 bool
}

// newACMEManager 创建 ACME 证书管理器。
func newACMEManager(cfg *Config) (*acmeManager, error) {
	if len(cfg.ACME.Domains) == 0 {
		return nil, fmt.Errorf("acme.domains 不能为空")
	}
	cacheDir := cfg.ACME.CacheDir
	if cacheDir == "" {
		cacheDir = "certs/acme"
	}
	mTLSFn, err := setupMTLS(cfg.ClientCA)
	if err != nil {
		return nil, err
	}
	m := &autocert.Manager{
		Cache:      autocert.DirCache(cacheDir),
		Prompt:     autocert.AcceptTOS,
		Email:      cfg.ACME.Email,
		HostPolicy: autocert.HostWhitelist(cfg.ACME.Domains...),
	}
	return &acmeManager{
		m:      m,
		mTLSFn: mTLSFn,
		http01: cfg.ACME.HTTP01,
	}, nil
}

// TLSConfig 返回 tls.Config，使用 autocert.Manager 的 GetCertificate。
func (m *acmeManager) TLSConfig() (*tls.Config, error) {
	tc := m.m.TLSConfig()
	if tc == nil {
		tc = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		// 确保最低 TLS 版本
		if tc.MinVersion == 0 {
			tc.MinVersion = tls.VersionTLS12
		}
	}
	if m.mTLSFn != nil {
		m.mTLSFn(tc)
	}
	// 如果启用 HTTP-01，启动挑战监听
	if m.http01 {
		go func() {
			slog.Info("ACME HTTP-01 challenge listener started on :80")
			if err := http.ListenAndServe(":80", m.m.HTTPHandler(nil)); err != nil {
				slog.Warn("ACME HTTP-01 listener stopped", "error", err)
			}
		}()
	}
	return tc, nil
}

// Ready 返回证书是否就绪。
func (m *acmeManager) Ready() bool { return true }

// Close 释放资源。
func (m *acmeManager) Close() error { return nil }
