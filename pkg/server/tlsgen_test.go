// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSelfSignedCert(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	err := GenerateSelfSignedCert(certFile, keyFile)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert() failed: %v", err)
	}

	// 验证 cert pem 存在且可解析
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("failed to read cert file: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode cert PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse cert: %v", err)
	}
	if cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("expected ECDSA public key, got %v", cert.PublicKeyAlgorithm)
	}

	// 验证 key pem 存在且可解析
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("failed to read key file: %v", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		t.Fatal("failed to decode key PEM block")
	}
	if keyBlock.Type != "EC PRIVATE KEY" {
		t.Errorf("expected EC PRIVATE KEY, got %s", keyBlock.Type)
	}
}

func TestGenerateSelfSignedCert_ReadOnlyDir(t *testing.T) {
	// 跳过：Windows 权限模型不同
	t.Skip("Windows 只读目录不影响文件创建")
}
