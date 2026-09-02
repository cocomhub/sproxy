// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// xferTestHandler 是单元测试用的占位 handler（xfer accept 循环只持有引用，
// 不真正路由；TLS/mux 握手装配正确性由集成测试覆盖）。
func xferTestHandler() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}

// genTestCertFiles 生成自签证书文件（cert.pem/key.pem），供 xfer_tls 装配测试使用。
func genTestCertFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	return certFile, keyFile
}

// TestStartXferListener_TLSEnabled 验证 xfer_tls 段装配成功：
// 返回 TLS listener 信息（地址非空、TLS=true、身份指纹非空），无错误。
func TestStartXferListener_TLSEnabled(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []server.AccessKeyConfig{
		{Key: testutil.TestAccessKey(), Secret: testutil.TestKey()},
	}
	certFile, keyFile := genTestCertFiles(t)
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	cfg.TLS.AutoTLS = false
	cfg.Hub.Transports.XferTLS.Enabled = true
	cfg.Hub.Transports.XferTLS.Listen = "127.0.0.1:0"
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	infos, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("startXferListener（xfer_tls）应成功: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("应返回 1 个 listener，实际 %d", len(infos))
	}
	info := infos[0]
	if info.Name != "xfer_tls" {
		t.Errorf("listener 名称应为 xfer_tls，实际 %q", info.Name)
	}
	if !info.TLS {
		t.Error("xfer_tls 段应标记 TLS=true")
	}
	if info.Addr == "" {
		t.Fatal("xfer_tls listener 地址不应为空")
	}
	if !strings.HasPrefix(info.Addr, "127.0.0.1:") {
		t.Errorf("xfer_tls 应绑 loopback，实际 %q", info.Addr)
	}
	if info.Fingerprint == "" || !strings.HasPrefix(info.Fingerprint, "sha256:") {
		t.Errorf("服务端身份指纹应为 sha256: 前缀，实际 %q", info.Fingerprint)
	}
}

// TestStartXferListener_PlainTCP 验证 xfer_tcp 明文段装配成功：
// 返回 TLS=false 的 listener（明文 tcp 仅显式 option）。
func TestStartXferListener_PlainTCP(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []server.AccessKeyConfig{
		{Key: testutil.TestAccessKey(), Secret: testutil.TestKey()},
	}
	cfg.Hub.Transports.XferTCP.Enabled = true
	cfg.Hub.Transports.XferTCP.Listen = "127.0.0.1:0"
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

	infos, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("startXferListener（xfer_tcp 明文）应成功: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("应返回 1 个 listener，实际 %d", len(infos))
	}
	if infos[0].TLS {
		t.Error("xfer_tcp 段未显式 tls_enabled 时应为明文（TLS=false）")
	}
	if infos[0].Addr == "" {
		t.Fatal("xfer_tcp listener 地址不应为空")
	}
}

// TestStartXferListener_Disabled 验证两个 xfer 段都未启用时无操作（不报错、不启动）。
func TestStartXferListener_Disabled(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []server.AccessKeyConfig{
		{Key: testutil.TestAccessKey(), Secret: testutil.TestKey()},
	}

	infos, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("xfer 未启用时 startXferListener 应无操作返回 nil: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("xfer 未启用时应返回空列表，实际 %d", len(infos))
	}
}

// TestStartXferListener_NoAccessKeysFails 验证 fail-closed：
// xfer listener 启用但无 access_keys → 拒启（隧道密钥派生失败，AD-3）。
func TestStartXferListener_NoAccessKeysFails(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Hub.Transports.XferTLS.Enabled = true
	cfg.Hub.Transports.XferTLS.Listen = "127.0.0.1:0"
	// 无 AccessKeys

	_, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err == nil {
		t.Fatal("xfer listener 无 access_keys 时应拒启（fail-closed）")
	}
	if !strings.Contains(err.Error(), "access_keys") {
		t.Errorf("错误应提及 access_keys，实际 %v", err)
	}
}

// TestStartXferListener_NoCertFails 验证 fail-closed：
// xfer_tls 启用但无任何证书（AutoTLS=false、cert_file 空）→ 拒启。
func TestStartXferListener_NoCertFails(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []server.AccessKeyConfig{
		{Key: testutil.TestAccessKey(), Secret: testutil.TestKey()},
	}
	cfg.TLS.AutoTLS = false
	cfg.TLS.CertFile = ""
	cfg.TLS.KeyFile = ""
	cfg.Hub.Transports.XferTLS.Enabled = true
	cfg.Hub.Transports.XferTLS.Listen = "127.0.0.1:0"
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

	_, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err == nil {
		t.Fatal("xfer_tls 无证书时应拒启（fail-closed）")
	}
}

// TestStartXferListener_XferTCPTLSUpgraded 验证 xfer_tcp 段显式 tls_enabled=true
// 时升级为 TLS listener（复用 tcp+tls 传输）。
func TestStartXferListener_XferTCPTLSUpgraded(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []server.AccessKeyConfig{
		{Key: testutil.TestAccessKey(), Secret: testutil.TestKey()},
	}
	certFile, keyFile := genTestCertFiles(t)
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	cfg.TLS.AutoTLS = false
	cfg.Hub.Transports.XferTCP.Enabled = true
	cfg.Hub.Transports.XferTCP.TLSEnabled = true
	cfg.Hub.Transports.XferTCP.Listen = "127.0.0.1:0"
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	infos, err := startXferListener(t.Context(), cfg, xferTestHandler(), testutil.DiscardLogger())
	if err != nil {
		t.Fatalf("startXferListener（xfer_tcp + tls_enabled）应成功: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("应返回 1 个 listener，实际 %d", len(infos))
	}
	if !infos[0].TLS {
		t.Error("xfer_tcp 段显式 tls_enabled=true 时应为 TLS listener")
	}
}
