// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certmgr

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// acmeManager 管理 ACME 自动证书。
type acmeManager struct {
	mu      sync.Mutex
	m       *autocert.Manager
	mTLSFn  func(*tls.Config)
	http01  bool
	httpSrv *http.Server // HTTP-01 挑战服务器，Close() 时关闭
}

// newACMEManager 创建 ACME 证书管理器。
// 注意：域名列表由 New() 保证非空后再调用此函数，但保留防御性校验。
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
	} else if tc.MinVersion == 0 {
		// 确保最低 TLS 版本
		tc.MinVersion = tls.VersionTLS12
	}
	if m.mTLSFn != nil {
		m.mTLSFn(tc)
	}
	// 如果启用 HTTP-01，启动挑战监听（互斥保护防止多次启动）
	m.mu.Lock()
	if m.http01 && m.httpSrv == nil {
		m.httpSrv = &http.Server{
			Addr:              ":80",
			Handler:           m.m.HTTPHandler(nil),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
		}
		go func() {
			slog.Info("ACME HTTP-01 challenge listener started on :80")
			if err := m.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Warn("ACME HTTP-01 listener stopped", "error", err)
			}
		}()
	}
	m.mu.Unlock()
	return tc, nil
}

// Ready 返回证书是否就绪。
func (m *acmeManager) Ready() bool { return true }

// Close 释放资源，关闭 HTTP-01 挑战服务器。
func (m *acmeManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.httpSrv.Shutdown(ctx)
	}
	return nil
}
