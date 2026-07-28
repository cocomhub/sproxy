// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certmgr

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestNew_FileCertManager(t *testing.T) {
	cfg := &Config{
		CertFile: "/path/to/cert.pem",
		KeyFile:  "/path/to/key.pem",
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if m == nil {
		t.Fatal("New() returned nil")
	}
	tc, err := m.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() failed: %v", err)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2, got %v", tc.MinVersion)
	}
	if !m.Ready() {
		t.Error("Ready() should return true")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() failed: %v", err)
	}
}

func TestNew_ACMEManager(t *testing.T) {
	cfg := &Config{
		ACME: ACMEConfig{
			Enabled: true,
			Domains: []string{"example.com", "www.example.com"},
			Email:   "admin@example.com",
		},
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if m == nil {
		t.Fatal("New() returned nil")
	}
	tc, err := m.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() failed: %v", err)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2, got %v", tc.MinVersion)
	}
	if tc.GetCertificate == nil {
		t.Error("expected GetCertificate to be set for ACME")
	}
	if !m.Ready() {
		t.Error("Ready() should return true")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() failed: %v", err)
	}
}

func TestNew_SelfSignedManager(t *testing.T) {
	// 使用临时目录作为工作目录，避免污染 CWD
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get CWD: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })

	cfg := &Config{
		AutoTLS: true,
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if m == nil {
		t.Fatal("New() returned nil")
	}

	// 使用默认路径，验证证书文件被创建
	sm, ok := m.(*selfSignedManager)
	if !ok {
		t.Fatal("expected *selfSignedManager type")
	}
	if _, err := os.Stat(sm.certFile); os.IsNotExist(err) {
		t.Errorf("cert file was not created at %s", sm.certFile)
	}
	if _, err := os.Stat(sm.keyFile); os.IsNotExist(err) {
		t.Errorf("key file was not created at %s", sm.keyFile)
	}

	tc, err := m.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() failed: %v", err)
	}
	if len(tc.Certificates) == 0 {
		t.Error("expected at least one certificate")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2, got %v", tc.MinVersion)
	}
	if !m.Ready() {
		t.Error("Ready() should return true")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() failed: %v", err)
	}
}

func TestNew_SelfSignedManager_WithCustomPath(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	// 先创建证书，再让 selfSignedManager 加载已存在的证书
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("failed to generate cert: %v", err)
	}

	// 使用 AutoTLS + 自定义路径，证书已存在所以不会重新生成
	cfg := &Config{
		AutoTLS:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
	}
	m, err := newSelfSignedManager(cfg)
	if err != nil {
		t.Fatalf("newSelfSignedManager() failed: %v", err)
	}
	if m == nil {
		t.Fatal("newSelfSignedManager() returned nil")
	}
	tc, err := m.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() failed: %v", err)
	}
	if len(tc.Certificates) == 0 {
		t.Error("expected at least one certificate")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2, got %v", tc.MinVersion)
	}
	if !m.Ready() {
		t.Error("Ready() should return true")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() failed: %v", err)
	}
}

func TestNew_NoConfig(t *testing.T) {
	cfg := &Config{}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error for empty config")
	}
}

func TestSetupMTLS(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")

	// Generate a self-signed cert to use as CA
	if err := GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("failed to generate cert: %v", err)
	}
	// Copy the cert as CA cert
	data, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("failed to read cert: %v", err)
	}
	if err := os.WriteFile(caFile, data, 0644); err != nil {
		t.Fatalf("failed to write CA file: %v", err)
	}

	cfg := &Config{
		AutoTLS:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
		ClientCA: caFile,
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	tc, err := m.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig() failed: %v", err)
	}
	if tc.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected ClientAuth RequireAndVerifyClientCert, got %v", tc.ClientAuth)
	}
	if tc.ClientCAs == nil {
		t.Error("expected ClientCAs to be set")
	}
}
