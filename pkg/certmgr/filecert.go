// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certmgr

import (
	"crypto/tls"
	"fmt"
)

// fileCertManager 管理静态文件证书。
type fileCertManager struct {
	certFile string
	keyFile  string
	mTLSFn   func(*tls.Config)
}

// newFileCertManager 创建文件证书管理器。
func newFileCertManager(cfg *Config) (*fileCertManager, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("cert_file 和 key_file 都必须指定")
	}
	mTLSFn, err := setupMTLS(cfg.ClientCA)
	if err != nil {
		return nil, err
	}
	return &fileCertManager{
		certFile: cfg.CertFile,
		keyFile:  cfg.KeyFile,
		mTLSFn:   mTLSFn,
	}, nil
}

// TLSConfig 返回 tls.Config，加载证书文件。
func (m *fileCertManager) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载文件证书失败: %w", err)
	}
	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if m.mTLSFn != nil {
		m.mTLSFn(tc)
	}
	return tc, nil
}

// Ready 返回证书是否就绪。
func (m *fileCertManager) Ready() bool { return true }

// Close 释放资源。
func (m *fileCertManager) Close() error { return nil }
