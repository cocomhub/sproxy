// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// xferIntegrationCfg 构造带 hub 语义（xfer_tls 启用 + access_keys）的完整 server.Config。
// 返回 cfg 与清理函数（关闭 handlers）。
func xferIntegrationCfg(t *testing.T) (*server.Config, *server.Handlers) {
	t.Helper()
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

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "v",
		BuildAt: "b",
		Logger:  testutil.DiscardLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })
	return cfg, h
}

// xferClientTLSConfig 构造客户端信任服务端证书的 *tls.Config（CertPool + ServerName，
// 复现生产 `sclient tunnel --xfer tcp+tls --ca-file` 语义）。
func xferClientTLSConfig(t *testing.T, certFile string) *tls.Config {
	t.Helper()
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("读取证书: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("证书池添加证书失败")
	}
	return &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	}
}

// TestXferListener_FileUploadList 端到端验证服务端 xfer TLS listener 全链路：
// sproxy（hub + access_keys + xfer_tls）→ FileClient.WithXfer("tcp+tls") →
// 经 mux → tunnel 解密 → 路由到本地文件 API，upload/list 均成功。
// 客户端 pinning 用服务端身份指纹（正确 pin → 握手成功）。
func TestXferListener_FileUploadList(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cfg, h := xferIntegrationCfg(t)

	infos, aerr := startXferListener(ctx, cfg, h.LocalHandler(), testutil.DiscardLogger())
	if aerr != nil {
		t.Fatalf("startXferListener: %v", aerr)
	}
	if len(infos) != 1 {
		t.Fatalf("应返回 1 个 xfer listener，实际 %d", len(infos))
	}
	addr := infos[0].Addr
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	// 客户端 TLS 配置（信任服务端证书）。
	builtin.SetDefaultTLSConfig(xferClientTLSConfig(t, cfg.TLS.CertFile))

	// 隧道密钥（与服务端同 AK/SK 派生，AD-3）。
	key, kerr := server.HubXferKey(cfg)
	if kerr != nil {
		t.Fatalf("HubXferKey: %v", kerr)
	}
	hexKey := hex.EncodeToString(key)

	// 客户端：WithXfer("tcp+tls") + 正确服务端身份指纹 pin。
	c := client.NewFileClient("https://127.0.0.1:1",
		client.WithXfer("tcp+tls", addr, hexKey),
		client.WithPeerFingerprints([]string{infos[0].Fingerprint}),
		client.WithTimeout(15*time.Second))
	if ierr := c.InitError(); ierr != nil {
		t.Fatalf("client init error: %v", ierr)
	}

	// 上传：xfer TLS 隧道 → 本地文件 API。
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "xfer-tls.txt")
	content := "xfer tls listener e2e — 证书 pinning 能力服务生产拓扑"
	if werr := os.WriteFile(srcPath, []byte(content), 0644); werr != nil {
		t.Fatalf("写源文件: %v", werr)
	}
	if _, uerr := c.Upload(ctx, srcPath, "xfer-tls.txt"); uerr != nil {
		t.Fatalf("经 xfer TLS 隧道上传失败: %v", uerr)
	}

	// 列表：确认文件可见。
	files, lerr := c.List(ctx)
	if lerr != nil {
		t.Fatalf("经 xfer TLS 隧道列目录失败: %v", lerr)
	}
	found := false
	for _, f := range files {
		if f.Name == "xfer-tls.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("列表应包含 xfer-tls.txt，实际 %+v", files)
	}
}

// TestXferListener_WrongClientPinFails 验证 fail-closed：客户端用错误服务端身份
// 指纹 pinning 连接 xfer TLS listener → 握手失败（客户端 TunnelDo 报错，
// 与 pkg/client 的 ErrPeerFingerprintMismatch 哨兵一致）。
func TestXferListener_WrongClientPinFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cfg, h := xferIntegrationCfg(t)

	infos, aerr := startXferListener(ctx, cfg, h.LocalHandler(), testutil.DiscardLogger())
	if aerr != nil {
		t.Fatalf("startXferListener: %v", aerr)
	}
	addr := infos[0].Addr
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })
	builtin.SetDefaultTLSConfig(xferClientTLSConfig(t, cfg.TLS.CertFile))

	key, kerr := server.HubXferKey(cfg)
	if kerr != nil {
		t.Fatalf("HubXferKey: %v", kerr)
	}
	hexKey := hex.EncodeToString(key)

	wrong, gerr := tunnel.GenerateIdentity()
	if gerr != nil {
		t.Fatalf("GenerateIdentity: %v", gerr)
	}

	// 客户端 pin 一个无关身份指纹。
	c := client.NewFileClient("https://127.0.0.1:1",
		client.WithXfer("tcp+tls", addr, hexKey),
		client.WithPeerFingerprints([]string{wrong.Fingerprint()}),
		client.WithTimeout(15*time.Second))

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/api/files", nil)
	resp, derr := c.TunnelDo(req)
	if derr == nil {
		resp.Body.Close()
		t.Fatal("错误 pin 时隧道请求应失败（fail-closed）")
	}
	if !errors.Is(derr, tunnel.ErrPeerFingerprintMismatch) {
		t.Errorf("错误应匹配 ErrPeerFingerprintMismatch，实际 %v", derr)
	}
}

// TestXferListener_WrongKeyFails 验证 C-1 核心修复：客户端用**错误静态密钥**（与
// 服务端派生隧道密钥不同）连接 xfer TLS listener，即使身份 pinning 正确（握手阶段
// 通过），数据面也必须失败——静态密钥参与会话密钥派生，两端 sessionKey 不同，
// 首个加密帧 AES-GCM 解密失败 → TunnelDo 报错（fail-closed，零凭据访问被拒）。
func TestXferListener_WrongKeyFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cfg, h := xferIntegrationCfg(t)

	infos, aerr := startXferListener(ctx, cfg, h.LocalHandler(), testutil.DiscardLogger())
	if aerr != nil {
		t.Fatalf("startXferListener: %v", aerr)
	}
	if len(infos) != 1 {
		t.Fatalf("应返回 1 个 xfer listener，实际 %d", len(infos))
	}
	addr := infos[0].Addr
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })
	builtin.SetDefaultTLSConfig(xferClientTLSConfig(t, cfg.TLS.CertFile))

	// 客户端用错误密钥（64 hex，合法但不同于服务端 HubXferKey 派生的密钥）。
	wrongHexKey := strings.Repeat("ab", 32)

	c := client.NewFileClient("https://127.0.0.1:1",
		client.WithXfer("tcp+tls", addr, wrongHexKey),
		client.WithPeerFingerprints([]string{infos[0].Fingerprint}),
		client.WithTimeout(15*time.Second))
	if ierr := c.InitError(); ierr != nil {
		t.Fatalf("client init error: %v", ierr)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/api/files", nil)
	resp, derr := c.TunnelDo(req)
	if derr == nil {
		resp.Body.Close()
		t.Fatal("错误静态密钥的客户端应被拒绝（C-1 验收：匿名 ECDH 让错误 key 也互通时此测试红）")
	}
}

// TestXferListener_ConfigSetDefaults 验证 SetDefaults 填充 xfer 默认监听地址
// （loopback），远程可达须显式 listen（安全边界，DoD 5）。
func TestXferListener_ConfigSetDefaults(t *testing.T) {
	cfg := server.Default()
	cfg.Hub.Transports.XferTLS.Enabled = true
	cfg.Hub.Transports.XferTLS.Listen = ""
	cfg.Hub.Transports.XferTCP.Enabled = true
	cfg.Hub.Transports.XferTCP.Listen = ""
	cfg.SetDefaults()

	if cfg.Hub.Transports.XferTLS.Listen == "" {
		t.Fatal("xfer_tls.listen 为空时应回落默认地址")
	}
	if !strings.HasPrefix(cfg.Hub.Transports.XferTLS.Listen, "127.0.0.1:") {
		t.Errorf("xfer_tls 默认应绑 loopback，实际 %q", cfg.Hub.Transports.XferTLS.Listen)
	}
	if cfg.Hub.Transports.XferTCP.Listen == "" {
		t.Fatal("xfer_tcp.listen 为空时应回落默认地址")
	}
	if !strings.HasPrefix(cfg.Hub.Transports.XferTCP.Listen, "127.0.0.1:") {
		t.Errorf("xfer_tcp 默认应绑 loopback，实际 %q", cfg.Hub.Transports.XferTCP.Listen)
	}
}
