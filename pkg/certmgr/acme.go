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
	mu         sync.Mutex
	m          *autocert.Manager
	mTLSFn     func(*tls.Config)
	http01     bool
	http01Addr string
	httpSrv    *http.Server // HTTP-01 挑战服务器，Close() 时关闭，置 nil 防止重复释放
	http01Done chan error   // 缓冲1，goroutine 退出时发送错误，Close() 等待
}

// http01Serve 启动 HTTP-01 挑战服务器（在 goroutine 中运行）。
// 退出时一定会向 http01Done 发送（nil 表示正常退出，非 nil 表示运行时错误）。
func (m *acmeManager) http01Serve(srv *http.Server) {
	slog.Info("ACME HTTP-01 challenge listener started", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Warn("ACME HTTP-01 listener stopped", "error", err)
		m.http01Done <- err
		return
	}
	m.http01Done <- nil
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
		m:          m,
		mTLSFn:     mTLSFn,
		http01:     cfg.ACME.HTTP01,
		http01Addr: cfg.ACME.HTTP01Port,
		http01Done: make(chan error, 1),
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
		addr := m.http01Addr
		if addr == "" {
			addr = ":80"
		}
		m.httpSrv = &http.Server{
			Addr:              addr,
			Handler:           m.m.HTTPHandler(nil),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
		}
		go m.http01Serve(m.httpSrv)
	}
	m.mu.Unlock()
	return tc, nil
}

// Ready 返回证书是否就绪。
// ACME 模式下证书通过 autocert 懒加载，首次 TLS 握手时获取，
// 因此始终返回 true。如需精确就绪状态可检查缓存目录。
func (m *acmeManager) Ready() bool { return true }

// Close 释放资源，关闭 HTTP-01 挑战服务器。
// 关闭后会等待 goroutine 退出并返回运行时错误（如果有）。
func (m *acmeManager) Close() error {
	m.mu.Lock()
	srv := m.httpSrv
	m.httpSrv = nil // 防止重复释放
	m.mu.Unlock()

	if srv == nil {
		return nil
	}

	// 关闭 HTTP-01 服务器，触发 ListenAndServe 返回
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(ctx)
	if shutdownErr != nil && shutdownErr != http.ErrServerClosed {
		// Shutdown 超时或失败时强制关闭 listener，确保 goroutine 退出
		_ = srv.Close()
	}

	// 始终等待 goroutine 退出并获取运行时错误
	// 注意：即使 Shutdown 失败，srv.Close() 后 ListenAndServe 也会返回
	if err := <-m.http01Done; err != nil {
		return fmt.Errorf("HTTP-01 服务器错误: %w", err)
	}

	// Shutdown 失败时返回原始错误，但 goroutine 已安全退出
	if shutdownErr != nil && shutdownErr != http.ErrServerClosed {
		return shutdownErr
	}
	return nil
}
