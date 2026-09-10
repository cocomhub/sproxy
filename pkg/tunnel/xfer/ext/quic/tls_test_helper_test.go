// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupQUICTLS 为当前测试注入一套可互相校验的 QUIC TLS 证书环境：
// 生成 CA 与由其签发的 leaf 证书，写入 t.TempDir()，并通过 t.Setenv 指向它们。
// 调用后 Listen 加载 leaf、Dial 用 CA 池校验，可完成真实的 TLS 校验握手
// （不放宽安全，不启用 InsecureSkipVerify）。
//
// 注意：t.Setenv 不能在已调用 t.Parallel 的测试中调用（会 panic）。
// 因此必须在未并行化的父测试里调用（如 TestQUIC、newQUICConnPair 的调用方）；
// 并行子测试在父测试体返回后才恢复执行，期间 env 始终有效。
func setupQUICTLS(t *testing.T) {
	t.Helper()
	certPath, keyPath, caPath := writeTestCertFiles(t, t.TempDir())
	t.Setenv("SPROXY_QUIC_CERT_FILE", certPath)
	t.Setenv("SPROXY_QUIC_KEY_FILE", keyPath)
	t.Setenv("SPROXY_QUIC_CA_CERT", caPath)
}

// writeTestCertFiles 生成 CA + leaf 证书对写入 dir，返回 leaf 证书、leaf 私钥、
// CA 证书三个文件路径。leaf 带 127.0.0.1 / ::1 / localhost SAN 与 ServerAuth 用途。
func writeTestCertFiles(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()

	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sproxy-quic-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "sproxy-quic-test-leaf"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	certPath = filepath.Join(dir, "leaf.pem")
	keyPath = filepath.Join(dir, "leaf-key.pem")
	caPath = filepath.Join(dir, "ca.pem")

	writePEMFile(t, caPath, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	writePEMFile(t, certPath, &pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	writePEMFile(t, keyPath, &pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER})
	return certPath, keyPath, caPath
}

func writePEMFile(t *testing.T, path string, block *pem.Block) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
