// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
)

// randSKBytes 生成 32B 随机 SK（测试用；cmd 包无共享 helper，本地定义）。
func randSKBytes(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// ---- trust login（task 10）----

// totpTestNonceCode 是 trust login 测试统一的动态码与 nonce（mock 服务端用同一
// (code,ak,nonce) 派生 wrap key，客户端 LoginTOTP 内部同源解开）。
const (
	totpTestCode  = "123456"
	totpTestNonce = "11223344556677889900aabbccddeeff"
)

// trustLoginEnv 是 trust login 命令测试的完整 mock 环境：
//   - srv：httptest 服务端（register/nonce/login 三端点按需注册远程行为）；
//   - cfgPath / cfg：隔离配置文件 + 初始配置（ServerURL=srv.URL）；
//   - svc：mock factory 返回的无凭据 FileClient（WithSendNoAuth 短路签名，M14）。
//
// 生成的 session SK（sessionSK）由 mock login 端点构造 KindTOTPWrap 信封包裹，断言
// 命令回填它就是解出的明文。
type trustLoginEnv struct {
	srv       *httptest.Server
	cfgPath   string
	cfg       *client.Config
	svc       *client.FileClient
	sessionSK []byte
	skeyID    string

	// gotBodies / gotAuths 记录收到的请求体（login_type 透传 D3 断言）与
	// Authorization 头（无凭据 M14 断言）。
	gotBodies []totpLoginBody
	gotAuths  []string
	regCalls  int
}

type totpLoginBody struct {
	AK        string `json:"ak"`
	Nonce     string `json:"nonce"`
	Code      string `json:"code"`
	LoginType string `json:"login_type"`
}

// newTrustLoginEnv 创建 trust login 测试环境。registerAdmin 为 true 时 register
// 响应 admin=true（S2 首 admin 提示分支）。
func newTrustLoginEnv(t *testing.T, registerAdmin bool) *trustLoginEnv {
	t.Helper()
	env := &trustLoginEnv{
		sessionSK: randSKBytes(t),
		skeyID:    "skey-login-aabbccdd",
	}
	// mock 服务端用同一 (code,ak,nonce) 派生 wrap key 包裹 session SK。
	wk, err := accesskey.DeriveTOTPWrapKey(totpTestCode, "ak-totp-0123456789abcdef", totpTestNonce)
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey: %v", err)
	}
	envelope, err := accesskey.EncryptSecretKind(accesskey.KindTOTPWrap, "ak-totp-0123456789abcdef", env.sessionSK, wk)
	if err != nil {
		t.Fatalf("EncryptSecretKind: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/credentials/register", func(w http.ResponseWriter, r *http.Request) {
		env.regCalls++
		env.gotAuths = append(env.gotAuths, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": "ak-totp-0123456789abcdef", "owner": "tenant-x",
			"admin": registerAdmin, "otpauth_uri": "otpauth://totp/demo?secret=AAAA&issuer=sproxy",
			"base32_secret": "JBSWY3DPEHPK3PXP",
		})
	})
	mux.HandleFunc("POST /api/credentials/nonce", func(w http.ResponseWriter, r *http.Request) {
		env.gotAuths = append(env.gotAuths, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"nonce": totpTestNonce, "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("POST /api/credentials/login", func(w http.ResponseWriter, r *http.Request) {
		env.gotAuths = append(env.gotAuths, r.Header.Get("Authorization"))
		defer r.Body.Close()
		var body totpLoginBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		env.gotBodies = append(env.gotBodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ak": "ak-totp-0123456789abcdef", "session_skey_id": env.skeyID,
			"session_expires_at": "2099-02-01T00:00:00Z", "wrapped_session_secret": envelope,
		})
	})
	env.srv = httptest.NewServer(mux)
	t.Cleanup(env.srv.Close)

	// 隔离配置文件。
	cfgPath := filepath.Join(t.TempDir(), "sclient.yaml")
	cfg := client.DefaultConfig()
	cfg.ServerURL = env.srv.URL
	env.cfgPath = cfgPath
	env.cfg = cfg

	// 无凭据客户端：零凭据构造 + 显式短路签名（M14）。防御性断言三字段为空。
	env.svc = client.NewFileClient(env.srv.URL, client.WithSendNoAuth(true))
	if env.svc.AccessKey() != "" || env.svc.AccessKeySecret() != "" || env.svc.AccessKeyID() != "" {
		t.Fatalf("TOTP 客户端应无凭据: ak=%q secret=%q id=%q", env.svc.AccessKey(), env.svc.AccessKeySecret(), env.svc.AccessKeyID())
	}
	return env
}

// execute 运行 trust login 命令（注入 stdin；写入隔离配置）。
func (e *trustLoginEnv) execute(t *testing.T, in string, args ...string) (*strings.Builder, *strings.Builder, error) {
	t.Helper()
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut, In: strings.NewReader(in)}
	cmd := NewCmdTrust(clientfactory.NewMock(e.svc, nil), ios, &testConfigProvider{cfg: e.cfg}, &e.cfgPath)
	cmd.SetArgs(append([]string{"login"}, args...))
	err := cmd.Execute()
	return &out, &errOut, err
}

// reload 重新加载隔离配置文件（断言回填结果）。
func (e *trustLoginEnv) reload(t *testing.T) *client.Config {
	t.Helper()
	cfg, err := client.LoadConfig(e.cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	return cfg
}

// assertNoAuthHeaders 断言所有 TOTP RPC 均未携带 Authorization 头（M14 无凭据客户端）。
func (e *trustLoginEnv) assertNoAuthHeaders(t *testing.T) {
	t.Helper()
	for i, a := range e.gotAuths {
		if a != "" {
			t.Errorf("RPC #%d 不应带 Authorization 头（M14 无凭据客户端）, got %q", i, a)
		}
	}
}

// TestTrustLogin_HappyPath 覆盖完整流程：注册 → nonce → login(cli) → 回填三件套。
// 断言：输出含 ak/base32 提示；login 请求 login_type=cli（D3）；RPC 无 Authorization
// 头（M14）；配置文件回填 access_key / access_key_secret(=session SK hex) /
// access_key_id(=session skey_id)。
func TestTrustLogin_HappyPath(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	out, _, err := env.execute(t, totpTestCode+"\n",
		"--owner", "tenant-x")
	if err != nil {
		t.Fatalf("trust login failed: %v", err)
	}

	// 注册分支：打印 ak 与 base32_secret（唯一展示）。
	o := out.String()
	if !strings.Contains(o, "ak-totp-0123456789abcdef") {
		t.Errorf("输出应含注册 AK, got: %s", o)
	}
	if !strings.Contains(o, "JBSWY3DPEHPK3PXP") {
		t.Errorf("输出应含 base32_secret（供录入 Authenticator）, got: %s", o)
	}
	if env.regCalls != 1 {
		t.Errorf("register 应恰好调用 1 次, got %d", env.regCalls)
	}

	// 断言请求体：login 请求 login_type=cli（D3 回填走 cli）。
	if len(env.gotBodies) != 1 {
		t.Fatalf("login 请求数 = %d, want 1", len(env.gotBodies))
	}
	if env.gotBodies[0].LoginType != "cli" {
		t.Errorf("login login_type = %q, want cli（D3）", env.gotBodies[0].LoginType)
	}
	if env.gotBodies[0].AK != "ak-totp-0123456789abcdef" || env.gotBodies[0].Nonce != totpTestNonce {
		t.Errorf("login body = %+v, want ak + nonce", env.gotBodies[0])
	}
	// 无 Authorization 头（M14 显式无凭据客户端）。
	env.assertNoAuthHeaders(t)

	// 回填三件套。
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("access_key 未回填: got %q", cfg.AccessKey)
	}
	if cfg.AccessKeySecret != hex.EncodeToString(env.sessionSK) {
		t.Errorf("access_key_secret 未回填为解密出的 session SK: got %q want %q",
			cfg.AccessKeySecret, hex.EncodeToString(env.sessionSK))
	}
	if cfg.AccessKeyID != env.skeyID {
		t.Errorf("access_key_id 未回填: got %q want %q", cfg.AccessKeyID, env.skeyID)
	}
}

// TestTrustLogin_FirstAdminHint（S2）：register 返回 admin=true → 输出含首 admin 提示。
func TestTrustLogin_FirstAdminHint(t *testing.T) {
	env := newTrustLoginEnv(t, true)
	out, _, err := env.execute(t, totpTestCode+"\n")
	if err != nil {
		t.Fatalf("trust login failed: %v", err)
	}
	if !strings.Contains(out.String(), "您是首个注册用户，将成为 admin") {
		t.Errorf("admin=true 时应提示首 admin, got: %s", out.String())
	}
}

// TestTrustLogin_RegisterFlag（--register 强制注册分支）：已配置 access_key 且
// --register → 仍走注册（不跳过），并回填为新注册的 AK/SK。
func TestTrustLogin_RegisterFlag(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	// 预置已有 access_key（但无 secret——非注册态触发也无从签名；--register 强制走注册）。
	env.cfg.AccessKey = "ak-old-0123456789abcdef"

	out, _, err := env.execute(t, totpTestCode+"\n", "--register")
	if err != nil {
		t.Fatalf("trust login --register failed: %v", err)
	}
	if env.regCalls != 1 {
		t.Errorf("--register 应强制执行注册, regCalls=%d", env.regCalls)
	}
	if !strings.Contains(out.String(), "ak-totp-0123456789abcdef") {
		t.Errorf("--register 输出应含新注册 AK, got: %s", out.String())
	}
	if env.gotBodies[0].LoginType != "cli" {
		t.Errorf("--register 后 login login_type = %q, want cli", env.gotBodies[0].LoginType)
	}
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("access_key 应回填新注册 AK, got %q", cfg.AccessKey)
	}
}

// TestTrustLogin_StdinEOF（M4 语义仿 errDeleteAKNotConfirmed）：stdin 立即 EOF（无
// 输入动态码）→ 非零退出（返回错误），不得把「未输入」当成功。
func TestTrustLogin_StdinEOF(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	_, _, err := env.execute(t, "")
	if err == nil {
		t.Fatal("trust login with EOF input: 应返回非零 error")
	}
	if !errors.Is(err, errLoginNotConfirmed) {
		t.Errorf("应返回 errLoginNotConfirmed, got: %v", err)
	}
	if env.regCalls != 1 {
		t.Errorf("无动态码输入也应完成注册, regCalls=%d", env.regCalls)
	}
	if len(env.gotBodies) != 0 {
		t.Errorf("无输入不应发出 login 请求, got %d", len(env.gotBodies))
	}
	// 配置不得回填。
	cfg := env.reload(t)
	if cfg.AccessKey != "" || cfg.AccessKeySecret != "" || cfg.AccessKeyID != "" {
		t.Errorf("EOF 中止后配置不应回填: %+v", cfg)
	}
}

// TestTrustLogin_ManualAK（M6）：无配置 access_key 时用 --ak 手动指定 → 走 login
// 分支（不触发注册）；mock 断言请求体 ak 正确。
func TestTrustLogin_ManualAK(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	out, _, err := env.execute(t, totpTestCode+"\n",
		"--ak", "ak-totp-0123456789abcdef")
	if err != nil {
		t.Fatalf("trust login --ak failed: %v", err)
	}
	if env.regCalls != 0 {
		t.Errorf("--ak 指定应跳过注册, regCalls=%d", env.regCalls)
	}
	if len(env.gotBodies) != 1 || env.gotBodies[0].AK != "ak-totp-0123456789abcdef" {
		t.Errorf("login body = %+v, want --ak 指定的 AK", env.gotBodies)
	}
	if !strings.Contains(out.String(), "登录成功") {
		t.Errorf("输出应含登录成功, got: %s", out.String())
	}
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" || cfg.AccessKeyID != env.skeyID {
		t.Errorf("--ak 登录后回填不正确: %+v", cfg)
	}
}

// TestTrustLogin_OverwriteConfirm_No（D4）：预置非空 access_key_secret → 输入 n →
// 中止非零退出、config 未被覆盖。
func TestTrustLogin_OverwriteConfirm_No(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	env.cfg.AccessKey = "ak-old-0123456789abcdef"
	env.cfg.AccessKeySecret = strings.Repeat("ab", 32)
	env.cfg.AccessKeyID = "skey-old-aaaaaaaa"
	// 初始配置落盘：D4 覆盖确认断言「磁盘上的旧凭据未被改写」。
	if err := client.SaveConfig(env.cfg, env.cfgPath); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	// 用已有凭据：预置非空 access_key_secret（可能来自 renew 的长命 SK）→ D4
	// 覆盖确认出现在动态码之后——stdin 首行为动态码、第二行为覆盖确认。
	_, _, err := env.execute(t, totpTestCode+"\nn\n")
	if err == nil {
		t.Fatal("拒绝覆盖时应返回非零 error")
	}
	if !errors.Is(err, errLoginOverwriteDenied) {
		t.Errorf("应返回 errLoginOverwriteDenied, got: %v", err)
	}
	// 覆盖被拒绝 → 配置未被改写。
	cfg := env.reload(t)
	if cfg.AccessKeySecret != strings.Repeat("ab", 32) || cfg.AccessKey != "ak-old-0123456789abcdef" {
		t.Errorf("拒绝覆盖后配置不应变化: %+v", cfg)
	}
}

// TestTrustLogin_OverwriteConfirm_Yes（D4）：输入 y → 覆盖确认通过 → 回填成功。
func TestTrustLogin_OverwriteConfirm_Yes(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	env.cfg.AccessKey = "ak-old-0123456789abcdef"
	env.cfg.AccessKeySecret = strings.Repeat("ab", 32)
	env.cfg.AccessKeyID = "skey-old-aaaaaaaa"
	if err := client.SaveConfig(env.cfg, env.cfgPath); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	_, _, err := env.execute(t, totpTestCode+"\ny\n")
	if err != nil {
		t.Fatalf("确认覆盖后 trust login failed: %v", err)
	}
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("确认覆盖后 access_key 应更新: got %q", cfg.AccessKey)
	}
	if cfg.AccessKeySecret != hex.EncodeToString(env.sessionSK) {
		t.Errorf("确认覆盖后 access_key_secret 应更新: got %q", cfg.AccessKeySecret)
	}
	if cfg.AccessKeyID != env.skeyID {
		t.Errorf("确认覆盖后 access_key_id 应更新: got %q", cfg.AccessKeyID)
	}
}

// TestTrustLogin_OverwriteFlag（D4 --overwrite）：跳过覆盖确认直接回填。
func TestTrustLogin_OverwriteFlag(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	env.cfg.AccessKey = "ak-old-0123456789abcdef"
	env.cfg.AccessKeySecret = strings.Repeat("ab", 32)
	env.cfg.AccessKeyID = "skey-old-aaaaaaaa"
	if err := client.SaveConfig(env.cfg, env.cfgPath); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	// stdin 只有动态码（无覆盖确认输入）——--overwrite 跳过确认。
	_, _, err := env.execute(t, totpTestCode+"\n", "--overwrite")
	if err != nil {
		t.Fatalf("trust login --overwrite failed: %v", err)
	}
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" || cfg.AccessKeySecret != hex.EncodeToString(env.sessionSK) {
		t.Errorf("--overwrite 后回填不正确: %+v", cfg)
	}
}

// TestTrustLogin_ConfigIsolation：只写目标配置文件，不触碰真实用户配置目录。
func TestTrustLogin_ConfigIsolation(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	cfgDir := filepath.Dir(env.cfgPath)
	_, _, err := env.execute(t, totpTestCode+"\n")
	if err != nil {
		t.Fatalf("trust login failed: %v", err)
	}
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "sclient.yaml" {
		t.Fatalf("expected only sclient.yaml in config dir, got %v", entries)
	}
}
