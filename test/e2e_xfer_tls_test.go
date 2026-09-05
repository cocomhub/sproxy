// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxy_test — 阶段 5 工作项 1 PR-5：xfer tcp+tls 真实二进制 e2e。
// 与 cmd/sproxy 集成测试（同进程 RegisterRoutes + startXferListener）互补：
// 本文件构建真实 sproxy 二进制并以子进程启动，验证 xfer_tls 全链路
// （sproxy 进程 ⇄ mux ⇄ tunnel ⇄ 本地文件 API）在真实进程边界下可用，
// 使 DoD「真实二进制全链路」进入 CI。
package sproxy_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// xferTLSEnv 是 startSPROXYWithXferTLS 返回的 xfer_tls 环境信息。
type xferTLSEnv struct {
	// baseURL 是主 HTTP listener 地址（仅 healthz 就绪门用；xfer 模式下客户端不直连）。
	baseURL string
	// xferAddr 是 xfer_tls listener 地址（host:port），供客户端 --hub / WithXfer 拨号。
	xferAddr string
	// certFile 是自签证书 PEM 路径（客户端 --ca-file / 信任池用）。
	certFile string
	// identity 是服务端 Ed25519 身份（预生成；指纹供客户端 WithPeerFingerprints pin）。
	identity *tunnel.Identity
}

// e2eFreePort 返回 127.0.0.1 上的一个空闲端口（bind :0 → close → 返回地址）。
// 与 startSPROXY/startHubSPROXY 的 S115 参考模式一致：close 到子进程 bind 的
// TOCTOU 窗口极小，且由 healthz 就绪轮询兜底。
func e2eFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找空闲端口: %v", err)
	}
	addr := l.Addr().String()
	l.Close() //nolint:staticcheck // close before rebind in child process is fine for tests
	return addr
}

// xferKeyFromAK 用 e2eTestAK/e2eTestSK 派生 xfer 隧道密钥（与服务端 HubXferKey /
// 客户端 factory 同一 tunnel.DeriveTunnelKey 实现，保证两端派生参数一致）。
func xferKeyFromAK(t *testing.T) string {
	t.Helper()
	mesh := tunnel.AccessKeyMesh(e2eTestAK)
	key, err := tunnel.DeriveTunnelKey(e2eTestSK, mesh)
	if err != nil {
		t.Fatalf("派生 xfer 隧道密钥失败: %v", err)
	}
	return hex.EncodeToString(key)
}

// xferE2EClientTLS 构造客户端信任服务端自签证书的 *tls.Config（CertPool + ServerName，
// 复现生产 `sclient tunnel --xfer tcp+tls --ca-file` 语义）。
func xferE2EClientTLS(t *testing.T, certFile string) *tls.Config {
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

// startSPROXYWithXferTLS 构建真实 sproxy 二进制并以子进程启动，配置：
//
//   - hub.enabled + hub.transports.xfer_tls（enabled + 显式 loopback listen）；
//   - tls.auto_tls: true（自签；为确定性显式提供 cert_file/key_file，文件证书优先）；
//   - access_keys 一对（e2eTestAK/e2eTestSK）；
//   - hub.xfer_identity_file 指向预生成的临时身份文件（指纹供客户端 pin）。
//
// 主 HTTP listener 保持明文（tls.enabled: false）——healthz 就绪门走 HTTP；
// xfer_tls listener 独立端口承载 TLS。返回 xferTLSEnv 与 cleanup（Kill+Wait，
// sync.Once 保护，与既有 e2e helper 一致）。
func startSPROXYWithXferTLS(t *testing.T) (xferTLSEnv, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binPath := e2eBinPath(t, "cmd/sproxy")

	// 自签证书（xfer_tls 与主 HTTP listener 共用证书源；显式文件避免 auto_tls
	// 默认写仓库 certs/ 目录污染工作区——BuildXferTLSConfig 文件证书优先）。
	certFile := filepath.Join(tmpDir, "cert.pem")
	keyFile := filepath.Join(tmpDir, "key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书: %v", err)
	}

	// 预生成服务端 Ed25519 身份：LoadXferIdentity 对已存在文件只加载不覆盖，
	// 因此本端可直接用同一身份计算指纹（供客户端 pinning）。
	identityPath := filepath.Join(tmpDir, "ident", "server-identity.json")
	identity, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatalf("生成服务端身份: %v", err)
	}
	if err := tunnel.SaveIdentity(identity, identityPath); err != nil {
		t.Fatalf("保存服务端身份: %v", err)
	}

	// 端口分配：xfer_tls 与主 HTTP 各取一个空闲端口。
	xferAddr := e2eFreePort(t)
	mainAddr := e2eFreePort(t)

	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatalf("创建 uploads 目录: %v", err)
	}
	// 凭据 store 化：access_keys 不再装配 Ring，须 pre-seed 使 e2eTestAK 被识别。
	seedCredentialStore(t, uploadsDir, e2eTestAK, e2eTestSK)

	// 路径用 filepath.ToSlash 归一（Windows 反斜杠会触发 YAML 双引号转义，
	// 前斜杠在 Go os 层全平台可用，与既有 helper 语义一致）。
	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	configContent := fmt.Sprintf(`tls:
  enabled: false
  auto_tls: true
  cert_file: %q
  key_file: %q
hub:
  enabled: true
  xfer_identity_file: %q
  transports:
    ws:
      enabled: true
    xfer_tls:
      enabled: true
      listen: %q
access_keys:
  - key: %q
    secret: %q
`,
		filepath.ToSlash(certFile), filepath.ToSlash(keyFile),
		filepath.ToSlash(identityPath), xferAddr,
		e2eTestAK, e2eTestSK)
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("写入临时配置: %v", err)
	}

	cmd := exec.Command(binPath, "--addr", mainAddr, "--storage-root", uploadsDir, "--config", configPath)
	cmd.Dir = e2eModuleRoot()
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 sproxy(xfer_tls): %v", err)
	}

	baseURL := fmt.Sprintf("http://%s", mainAddr)
	cleanup := newKillWaitCleanup(cmd)

	// 就绪门：healthz（startXferListener 在 HTTP listener 启动前同步绑定，
	// healthz 可达即 xfer_tls listener 已就绪）。
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		cleanup()
		t.Fatalf("sproxy(xfer_tls) 未就绪; stdout:\n%s\nstderr:\n%s", stdoutBuf.String(), stderrBuf.String())
	}

	return xferTLSEnv{
		baseURL:  baseURL,
		xferAddr: xferAddr,
		certFile: certFile,
		identity: identity,
	}, cleanup
}

// newXferTLSClient 构造经 xfer tcp+tls 隧道访问本地文件 API 的 FileClient。
// hexKey 非空时派生隧道密钥；fingerprint 非空时 pin 服务端 Ed25519 身份。
// baseURL 传哑地址（xfer 模式不走 HTTP 直连，TunnelDo 经 mux 拨号）。
func newXferTLSClient(t *testing.T, env xferTLSEnv, hexKey, fingerprint string) *client.FileClient {
	t.Helper()
	opts := []client.Option{
		client.WithXfer("tcp+tls", env.xferAddr, hexKey),
		client.WithTimeout(15 * time.Second),
	}
	if fingerprint != "" {
		opts = append(opts, client.WithPeerFingerprints([]string{fingerprint}))
	}
	c := client.NewFileClient("https://127.0.0.1:1", opts...)
	if ierr := c.InitError(); ierr != nil {
		t.Fatalf("客户端初始化失败: %v", ierr)
	}
	return c
}

// TestE2E_XferTLS_FileUploadList 验证 xfer_tls 真实二进制全链路：
// sproxy 子进程（hub + xfer_tls + access_keys + 预生成身份）→ FileClient
// WithXfer("tcp+tls") + 自签证书信任 + 服务端身份指纹 pin → 经 mux → tunnel
// 解密 → 路由到本地文件 API，upload/list 均成功（C-1 正向验收）。
func TestE2E_XferTLS_FileUploadList(t *testing.T) {
	env, cleanup := startSPROXYWithXferTLS(t)
	defer cleanup()

	// 客户端 TLS 配置（信任服务端自签证书；与 sclient --ca-file 语义一致）。
	builtin.SetDefaultTLSConfig(xferE2EClientTLS(t, env.certFile))
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	c := newXferTLSClient(t, env, xferKeyFromAK(t), env.identity.Fingerprint())

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "xfer-tls-e2e.txt")
	content := "xfer tls real-binary e2e"
	if err := os.WriteFile(srcPath, []byte(content), 0644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	if _, err := c.Upload(ctx, srcPath, "xfer-tls-e2e.txt"); err != nil {
		t.Fatalf("经 xfer_tls 隧道上传失败: %v", err)
	}

	files, err := c.List(ctx)
	if err != nil {
		t.Fatalf("经 xfer_tls 隧道列表失败: %v", err)
	}
	found := false
	for _, f := range files {
		if f.Name == "xfer-tls-e2e.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("列表应包含 xfer-tls-e2e.txt，实际 %+v", files)
	}
}

// TestE2E_XferTLS_WrongKeyFails 验证 C-1 核心修复的 e2e 层面：客户端用错误静态密钥
// （与服务端派生隧道密钥不同）连接 xfer_tls listener，即使身份 pinning 正确
// （TLS 握手通过），数据面也必须失败——静态密钥参与会话密钥派生，两端 sessionKey
// 不同，首个加密帧 AES-GCM 解密失败 → TunnelDo 报错（fail-closed，零凭据被拒）。
func TestE2E_XferTLS_WrongKeyFails(t *testing.T) {
	env, cleanup := startSPROXYWithXferTLS(t)
	defer cleanup()

	builtin.SetDefaultTLSConfig(xferE2EClientTLS(t, env.certFile))
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	// 错误静态密钥：合法 64 hex（32 字节），但不同于服务端 HubXferKey 派生的密钥。
	c := newXferTLSClient(t, env, strings.Repeat("ab", 32), env.identity.Fingerprint())

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/api/files", nil)
	resp, err := c.TunnelDo(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("错误静态密钥的客户端应被拒绝（C-1 验收：数据面 fail-closed）")
	}
	// 审查 Minor #1：钉死失败发生在**数据面**（sessionKey 不一致 → 首帧解密失败 →
	// 服务端关流 → 客户端读响应 metadata 得 EOF），而非 TLS/身份握手层。CA 与指纹
	// 正确使握手必然通过，若错误不含数据面特征说明 C-1 语义回归（或路径漂移）。
	if !strings.Contains(err.Error(), "resp meta") {
		t.Fatalf("错误应来自数据面响应解密（resp meta），实际: %v（握手层失败说明路径漂移）", err)
	}
	t.Logf("错误 key 被拒绝（符合预期，数据面 fail-closed）: %v", err)
}

// TestE2E_XferTLS_SClientCLI 验证真实 sclient 二进制经 xfer tcp+tls 隧道访问文件 API：
//
//	先经 FileClient 上传一文件（复用 SDK 全链路），再运行
//	`sclient tunnel --xfer tcp+tls --hub <addr> --ca-file <cert> /api/files`，
//	断言隧道响应（文件列表 JSON）包含已上传文件名。
//
// 覆盖 sclient 子进程装配（--ca-file 自签信任、--hub host:port 校验、
// access_key/access_key_secret 派生隧道密钥）在真实进程边界下可用。
func TestE2E_XferTLS_SClientCLI(t *testing.T) {
	env, cleanup := startSPROXYWithXferTLS(t)
	defer cleanup()

	// 先用 FileClient 上传一个文件（复用 SDK 全链路），再经真实 sclient 二进制隧道列出。
	builtin.SetDefaultTLSConfig(xferE2EClientTLS(t, env.certFile))
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })

	fc := newXferTLSClient(t, env, xferKeyFromAK(t), env.identity.Fingerprint())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "sclient-cli-xfer.txt")
	if err := os.WriteFile(srcPath, []byte("sclient cli xfer tls e2e"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Upload(ctx, srcPath, "sclient-cli-xfer.txt"); err != nil {
		t.Fatalf("FileClient 经 xfer_tls 上传失败: %v", err)
	}

	// 真实 sclient 二进制：tunnel --xfer tcp+tls --hub <addr> --ca-file <cert> /api/files。
	binPath := e2eBinPath(t, "cmd/sclient")
	cliDir := t.TempDir()
	// 配置/缓存隔离：--config 指向临时配置 + 隔离 XDG，避免加载本地
	// ~/.config/sproxy/sclient.yaml 或用户身份文件干扰（memory: cli-test-config-isolation）。
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(cliDir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cliDir, "cache"))
	cfgPath := filepath.Join(cliDir, "sclient.yaml")
	cfgContent := fmt.Sprintf("server_url: %s\naccess_key: %s\naccess_key_secret: %s\n",
		env.baseURL, e2eTestAK, e2eTestSK)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(cliDir, "out.json")
	args := []string{
		"tunnel", "/api/files",
		"--config", cfgPath,
		"--xfer", "tcp+tls",
		"--hub", env.xferAddr,
		"--ca-file", env.certFile,
		"--output", outPath,
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = e2eModuleRoot()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sclient tunnel --xfer tcp+tls 失败: %v\n%s", err, out)
	}
	data, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("读取隧道响应文件: %v", rerr)
	}
	if !strings.Contains(string(data), "sclient-cli-xfer.txt") {
		t.Errorf("sclient tunnel /api/files 输出应包含已上传文件，got: %s", data)
	}
}
