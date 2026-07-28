// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certmgr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// selfSignedManager 管理自签证书。
type selfSignedManager struct {
	certFile string
	keyFile  string
	mTLSFn   func(*tls.Config)
}

// newSelfSignedManager 创建自签证书管理器。
// 如果证书文件不存在，则自动生成。
func newSelfSignedManager(cfg *Config) (*selfSignedManager, error) {
	certFile := cfg.CertFile
	keyFile := cfg.KeyFile
	if certFile == "" && keyFile == "" {
		certFile = "certs/_wildcard.sproxy.local.pem"
		keyFile = "certs/_wildcard.sproxy.local-key.pem"
	} else if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("cert_file 和 key_file 必须同时设置或同时为空")
	}
	// 检查证书文件是否存在，只要任一缺失就重新生成
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		// keyFile 存在但 certFile 不存在的情况已在上面处理
		// 这里处理 certFile 存在但 keyFile 不存在的情况
		if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
			return nil, err
		}
	}
	mTLSFn, err := setupMTLS(cfg.ClientCA)
	if err != nil {
		return nil, err
	}
	return &selfSignedManager{
		certFile: certFile,
		keyFile:  keyFile,
		mTLSFn:   mTLSFn,
	}, nil
}

// TLSConfig 返回 tls.Config，加载已生成的自签证书。
func (m *selfSignedManager) TLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载自签证书失败: %w", err)
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
func (m *selfSignedManager) Ready() bool { return true }

// Close 释放资源。
func (m *selfSignedManager) Close() error { return nil }

// GenerateSelfSignedCert 生成 ECDSA P-256 自签证书并写入 PEM 编码的证书和密钥文件。
// 如果父目录不存在，会自动创建。
func GenerateSelfSignedCert(certFile, keyFile string) error {
	// Generate ECDSA P-256 private key
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ECDSA key: %w", err)
	}

	// Generate a random serial number
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "sproxy.local",
			Organization: []string{"Cocomhub"},
		},
		NotBefore: now,
		NotAfter:  now.Add(10 * 365 * 24 * time.Hour), // 10 years

		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		DNSNames:    []string{"localhost", "sproxy.local"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	// Self-sign
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// Ensure parent directory exists
	dir := filepath.Dir(certFile)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create cert directory %s: %w", dir, err)
	}

	// Write cert PEM
	certOut, err := os.Create(certFile)
	if err != nil {
		return fmt.Errorf("create cert file %s: %w", certFile, err)
	}
	defer certOut.Close()

	if err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("encode cert PEM: %w", err)
	}

	// Write key PEM with restricted permissions (0600)
	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create key file %s: %w", keyFile, err)
	}
	defer keyOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal EC private key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encode key PEM: %w", err)
	}

	return nil
}
