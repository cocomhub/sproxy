// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- trust login --ca-file / --insecure（直连面安全 flag）----

// TestTrustLogin_CARegisterSuccess 验证：trust login --ca-file <自签CA> 能连上自签
// HTTPS 服务端完成注册（红灯：现实现 noAuth 客户端不消费 --ca-file → 注册请求
// TLS 握手失败）。
func TestTrustLogin_CARegisterSuccess(t *testing.T) {
	t.Parallel()
	srv := newTrustLoginTLSServer(t, false)
	caFile := writeTLSServerCA(t, srv)

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")
	cfg := newTrustLoginTLSConfig(srv)
	svc := newTrustLoginTLSClient(t, cfg, srv, false)

	cmd := newTrustLoginCmd(t, cfg, cfgPath, svc)
	setLoginFlag(t, cmd, "ca-file", caFile)
	cmd.SetArgs([]string{"login", "--register"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust login --ca-file 应成功（红灯: %v）", err)
	}
}

// TestTrustLogin_InsecureLoopbackSuccess 验证：trust login --insecure 连自签服务端
// 完成注册（loopback 自签场景兜底）。
func TestTrustLogin_InsecureLoopbackSuccess(t *testing.T) {
	t.Parallel()
	srv := newTrustLoginTLSServer(t, false)
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")
	cfg := newTrustLoginTLSConfig(srv)
	svc := newTrustLoginTLSClient(t, cfg, srv, true)

	cmd := newTrustLoginCmd(t, cfg, cfgPath, svc)
	setLoginFlag(t, cmd, "insecure", "true")
	cmd.SetArgs([]string{"login", "--register"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trust login --insecure 应成功（红灯: %v）", err)
	}
}

// TestTrustLogin_NoCA_Fails 验证对照：不配 CA/insecure 连自签服务端 → 注册请求
// TLS 握手失败（证明 CA/insecure 是让测试从红变绿的唯一开关）。
func TestTrustLogin_NoCA_Fails(t *testing.T) {
	t.Parallel()
	srv := newTrustLoginTLSServer(t, false)
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")
	cfg := newTrustLoginTLSConfig(srv)
	svc := newTrustLoginTLSClient(t, cfg, srv, false)

	cmd := newTrustLoginCmd(t, cfg, cfgPath, svc)
	cmd.SetArgs([]string{"login", "--register"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("不配 CA/insecure 连自签服务端应失败（fail-closed）")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("错误应指明证书校验失败, got: %v", err)
	}
}

// ---- helpers ----

// newTrustLoginTLSServer 启动自签 HTTPS TOTP mock 服务端（register 直接返回成功，
// 不走到 nonce/login——注册分支即验证 TLS 链路）。
func newTrustLoginTLSServer(t *testing.T, admin bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/register", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": "ak-totp-tls-test", "owner": "tls",
			"admin": admin, "otpauth_uri": "otpauth://totp/demo?secret=AAAA",
			"base32_secret": "JBSWY3DPEHPK3PXP",
		})
	})
	mux.HandleFunc("POST /api/credentials/nonce", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nonce": "11223344556677889900aabbccddeeff", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("POST /api/credentials/login", func(w http.ResponseWriter, r *http.Request) {
		sk := make([]byte, 32)
		if _, err := rand.Read(sk); err != nil {
			t.Errorf("rand.Read: %v", err)
			return
		}
		wk, err := accesskey.DeriveTOTPWrapKey("123456", "ak-totp-tls-test", "11223344556677889900aabbccddeeff")
		if err != nil {
			t.Errorf("DeriveTOTPWrapKey: %v", err)
			return
		}
		env, err := accesskey.EncryptSecretKind(accesskey.KindTOTPWrap, "ak-totp-tls-test", sk, wk)
		if err != nil {
			t.Errorf("EncryptSecretKind: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": "ak-totp-tls-test", "session_skey_id": "skey-tls-test",
			"session_expires_at": "2099-02-01T00:00:00Z", "wrapped_session_secret": env,
		})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeTLSServerCA 把 httptest TLS 服务端证书写为 CA 文件并返回路径。
func writeTLSServerCA(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	certPEM := pemEncodeCert(srv.Certificate().Raw)
	caFile := filepath.Join(t.TempDir(), "srv-ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile
}

// pemEncodeCert 包装 certmgr 证书 PEM 编码（避免重复依赖 encoding/pem）。
func pemEncodeCert(der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	return buf.Bytes()
}

// newTrustLoginTLSConfig 构造指向 TLS 服务端的隔离配置。
func newTrustLoginTLSConfig(srv *httptest.Server) *client.Config {
	cfg := client.DefaultConfig()
	cfg.ServerURL = srv.URL
	return cfg
}

// newTrustLoginTLSClient 构造 trust login 的 mock svc：无凭据 + 可选 insecure。
// 注意：mockFactory.NewClient 直接返回该 svc，不经过 factory 装配——因此本测试
// 只验证「trust login 自身构造 noAuth 客户端时应用 ca-file/insecure」。
func newTrustLoginTLSClient(t *testing.T, cfg *client.Config, srv *httptest.Server, insecure bool) *client.FileClient {
	t.Helper()
	var opts []client.Option
	opts = append(opts, client.WithSendNoAuth(true))
	if insecure {
		opts = append(opts, client.WithInsecureTLS())
	}
	svc := client.NewFileClient(srv.URL, opts...)
	return svc
}

// newTrustLoginCmd 构造 trust login 命令（mock factory + 隔离配置路径）。
func newTrustLoginCmd(t *testing.T, cfg *client.Config, cfgPath string, svc *client.FileClient) *cobra.Command {
	t.Helper()
	ios := cli.IOStreams{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}, In: strings.NewReader("123456\n")}
	trust := NewCmdTrust(clientfactory.NewMock(svc, nil), ios, &testConfigProvider{cfg: cfg}, &cfgPath)
	trust.PersistentFlags().String("ca-file", "", "")
	trust.PersistentFlags().Bool("insecure", false, "")
	trust.SetOut(&bytes.Buffer{})
	trust.SetErr(&bytes.Buffer{})
	return trust
}

// setLoginFlag 设置 trust login 的全局/本地 flag（PersistentPreRunE 未跑，直接 Set）。
func setLoginFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	f := cmd.PersistentFlags().Lookup(name)
	if f == nil {
		f = cmd.Flags().Lookup(name)
	}
	if f == nil {
		t.Fatalf("flag %s 未注册", name)
	}
	if err := f.Value.Set(value); err != nil {
		t.Fatal(err)
	}
}
