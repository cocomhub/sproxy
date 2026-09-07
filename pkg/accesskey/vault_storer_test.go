// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 任务 1 的 L1 单元测试：VaultTransitStorer 核心 Encrypt/Decrypt + AAD context + 错误分类。
// 测试内联 mock Vault（mockVaultServer，httptest 回放固定 JSON）；共享 vaultmock 工具
// 待任务 4 装配需要时才抽到 testutil。

const (
	vaultTestToken = "test-vault-token"
	vaultTestMount = "transit"
	vaultTestKey   = "sproxy"
	vaultTestAAD   = "credentials.json"
)

// mockVaultServer 返回一个模拟 Vault Transit encrypt/decrypt 端点的 httptest.Server。
// 默认行为：
//   - encrypt → {"data":{"ciphertext":"vault:v1:"+base64(明文)}}（明文自描述，支持全链路往返）；
//   - decrypt → {"data":{"plaintext":base64(明文)}}；明文由 decryptFn 定制，nil 时默认
//     解码 "vault:v1:" 后缀 base64（镜像 encrypt 产物）。
//
// 同时记录每次请求的 path / X-Vault-Token / body（-race 安全），供测试断言入参：
// X-Vault-Token 头、body 的 plaintext/context/ciphertext 字段。
type mockVaultServer struct {
	t     *testing.T
	token string

	srv *httptest.Server

	mu                sync.Mutex
	paths             []string         // 每次请求的 URL path
	tokens            []string         // 每次请求的 X-Vault-Token 头
	bodies            []map[string]any // 每次请求的 JSON body（解码后）
	overrides         map[string]vaultMockResp
	encryptCiphertext string // 非空 → encrypt 固定回此 data.ciphertext（原样透传断言）
	decryptFn         func(ciphertext string) string
}

// vaultMockResp 是一次性的覆写响应（错误/重定向场景：403/404/503/5xx/302 等）。
type vaultMockResp struct {
	status  int
	body    string
	headers map[string]string // 额外响应头（如 302 的 Location）
}

// newMockVault 创建 mock Vault server，并注册 t.Cleanup 关闭。
func newMockVault(t *testing.T, token string) *mockVaultServer {
	t.Helper()
	m := &mockVaultServer{t: t, token: token, overrides: map[string]vaultMockResp{}}
	m.srv = httptest.NewServer(m)
	t.Cleanup(m.srv.Close)
	return m
}

// URL 返回 mock 服务地址（与真实 Vault 兼容的 http://127.0.0.1:port 基址）。
func (m *mockVaultServer) URL() string { return m.srv.URL }

// override 为指定操作（"encrypt"/"decrypt"）设置固定状态码 + body 的覆写响应。
func (m *mockVaultServer) override(op string, status int, body string) {
	m.overrideResp(op, vaultMockResp{status: status, body: body})
}

// overrideResp 设置完整覆写响应（含响应头，如 302 的 Location）。
func (m *mockVaultServer) overrideResp(op string, resp vaultMockResp) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overrides[op] = resp
}

// newMockVaultTLS 创建启用 TLS 的 mock Vault server（httptest.NewTLSServer），并把其自签
// CA 导出为临时 PEM 路径返回——供 CAFile 正路径测试验证 RootCAs/Transport 装配。
func newMockVaultTLS(t *testing.T, token string) (*mockVaultServer, string) {
	t.Helper()
	m := &mockVaultServer{t: t, token: token, overrides: map[string]vaultMockResp{}}
	m.srv = httptest.NewTLSServer(m)
	t.Cleanup(m.srv.Close)
	caPath := filepath.Join(t.TempDir(), "vault-ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("导出 mock CA 到临时文件: %v", err)
	}
	return m, caPath
}

// setEncryptCiphertext 固定 encrypt 端点回放的 data.ciphertext（原样透传断言）。
func (m *mockVaultServer) setEncryptCiphertext(ct string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.encryptCiphertext = ct
}

// setDecryptFn 定制 decrypt 明文的生成逻辑（入参为收到的 ciphertext 字段值）。
func (m *mockVaultServer) setDecryptFn(fn func(ciphertext string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decryptFn = fn
}

// reqCount 返回收到的请求总数。
func (m *mockVaultServer) reqCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.paths)
}

// path 返回第 i 次请求的 URL path。
func (m *mockVaultServer) path(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.paths) {
		return ""
	}
	return m.paths[i]
}

// reqToken 返回第 i 次请求的 X-Vault-Token 头。
func (m *mockVaultServer) reqToken(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.tokens) {
		return ""
	}
	return m.tokens[i]
}

// body 返回第 i 次请求的 JSON body（解码后），越界返回 nil。
func (m *mockVaultServer) body(i int) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.bodies) {
		return nil
	}
	out := make(map[string]any, len(m.bodies[i]))
	maps.Copy(out, m.bodies[i])
	return out
}

// ServeHTTP 实现 mock Vault Transit 端点。断言 token 头；命中 override 时回放覆写响应；
// 否则按 encrypt/decrypt 默认行为回放 200 JSON。错误通过 t.Errorf 上报（handler 运行在
// httptest server goroutine，禁用 t.Fatalf）。
func (m *mockVaultServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		m.t.Errorf("mock vault: 读取请求体失败: %v", err)
		return
	}

	m.mu.Lock()
	m.paths = append(m.paths, r.URL.Path)
	m.tokens = append(m.tokens, r.Header.Get("X-Vault-Token"))
	m.bodies = append(m.bodies, decodeJSONBody(bodyBytes))
	ov, hasOverride := m.overrides[vaultMockOp(r.URL.Path)]
	fixedCT := m.encryptCiphertext
	fn := m.decryptFn
	m.mu.Unlock()

	if r.Method != http.MethodPost {
		m.t.Errorf("mock vault: method = %s, want POST", r.Method)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		m.t.Errorf("mock vault: Content-Type = %q, want application/json", ct)
	}
	if got := r.Header.Get("X-Vault-Token"); got != m.token {
		m.t.Errorf("mock vault: X-Vault-Token = %q, want %q", got, m.token)
	}
	if hasOverride {
		for k, v := range ov.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(ov.status)
		_, _ = io.WriteString(w, ov.body)
		return
	}

	var req struct {
		Plaintext  string `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
		Context    string `json:"context"`
	}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			m.t.Errorf("mock vault: 解析请求体 JSON 失败: %v", err)
		}
	}

	switch vaultMockOp(r.URL.Path) {
	case "encrypt":
		ct := fixedCT
		if ct == "" {
			pt, err := base64.StdEncoding.DecodeString(req.Plaintext)
			if err != nil {
				mockVaultError(w, http.StatusBadRequest, "bad plaintext base64")
				return
			}
			ct = "vault:v1:" + base64.StdEncoding.EncodeToString(pt)
		}
		mockVaultData(w, map[string]string{"ciphertext": ct})
	case "decrypt":
		plaintext := ""
		if fn != nil {
			plaintext = fn(req.Ciphertext)
		} else {
			pt, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Ciphertext, "vault:v1:"))
			if err != nil {
				mockVaultError(w, http.StatusBadRequest, "bad ciphertext base64")
				return
			}
			plaintext = string(pt)
		}
		mockVaultData(w, map[string]string{"plaintext": base64.StdEncoding.EncodeToString([]byte(plaintext))})
	default:
		mockVaultError(w, http.StatusNotFound, `unknown endpoint`)
	}
}

// vaultMockOp 从 URL path 提取 Transit 操作名（encrypt/decrypt），无法识别返回空串。
func vaultMockOp(path string) string {
	switch {
	case strings.Contains(path, "/encrypt/"):
		return "encrypt"
	case strings.Contains(path, "/decrypt/"):
		return "decrypt"
	}
	return ""
}

// decodeJSONBody 把请求体解码为 map；空体/非法 JSON 返回 nil（不中断请求处理）。
func decodeJSONBody(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}

// mockVaultData 以 {"data":{...}} 形态回 200 JSON（对齐 Vault Transit 成功响应）。
func mockVaultData(w http.ResponseWriter, data map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// mockVaultError 以 {"errors":[...]} 形态回错误 JSON（对齐 Vault 错误响应结构）。
func mockVaultError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{msg}})
}

// newTestVaultStorer 用固定测试参数构造 VaultTransitStorer（失败即终止测试）。
func newTestVaultStorer(t *testing.T, mock *mockVaultServer, aadPath string) *VaultTransitStorer {
	t.Helper()
	s, err := NewVaultTransitStorer(VaultOptions{
		Addr:    mock.URL(),
		Mount:   vaultTestMount,
		KeyName: vaultTestKey,
		Token:   vaultTestToken,
		AADPath: aadPath,
	})
	if err != nil {
		t.Fatalf("NewVaultTransitStorer: %v", err)
	}
	return s
}

// TestVaultTransitStorer_Encrypt_SendsTokenAndContext 验证 Encrypt 核心契约：
//   - 请求发往 /v1/{mount}/encrypt/{key}，携带 X-Vault-Token 头；
//   - body.plaintext == base64(明文)、body.context == base64(AADPath)；
//   - 返回密文 = Vault 响应的 data.ciphertext 原样（含 vault:v1: 前缀）。
func TestVaultTransitStorer_Encrypt_SendsTokenAndContext(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	const plaintext = "secret-data"
	ct, err := s.Encrypt([]byte(plaintext))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.HasPrefix(ct, []byte("vault:v1:")) {
		t.Fatalf("密文应含 vault:v1: 前缀, got %q", ct)
	}
	wantCT := "vault:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext))
	if string(ct) != wantCT {
		t.Fatalf("密文应等于 mock 回放的 data.ciphertext, got %q want %q", ct, wantCT)
	}
	if n := mock.reqCount(); n != 1 {
		t.Fatalf("Encrypt 应恰好发 1 次请求, got %d", n)
	}
	if got := mock.reqToken(0); got != vaultTestToken {
		t.Fatalf("X-Vault-Token 应为 %q, got %q", vaultTestToken, got)
	}
	if got := mock.path(0); !strings.HasSuffix(got, "/v1/transit/encrypt/sproxy") {
		t.Fatalf("请求路径应为 /v1/transit/encrypt/sproxy 结尾, got %q", got)
	}
	body := mock.body(0)
	if body["plaintext"] != base64.StdEncoding.EncodeToString([]byte(plaintext)) {
		t.Fatalf("body.plaintext 应为 base64(%q), got %q", plaintext, body["plaintext"])
	}
	if body["context"] != base64.StdEncoding.EncodeToString([]byte(vaultTestAAD)) {
		t.Fatalf("body.context 应为 base64(%q), got %q", vaultTestAAD, body["context"])
	}
}

// TestVaultTransitStorer_Decrypt_Roundtrip 验证 Decrypt 核心契约：
//   - 请求发往 /v1/{mount}/decrypt/{key}，携带 X-Vault-Token 头；
//   - body.ciphertext 原样上送、body.context == base64(AADPath)；
//   - 响应 data.plaintext 经 base64 解码后还原明文。
func TestVaultTransitStorer_Decrypt_Roundtrip(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	const original = "decrypted-原文-凭证"
	mock.setDecryptFn(func(ciphertext string) string {
		return original
	})
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	const ct = "vault:v1:encrypted"
	pt, err := s.Decrypt([]byte(ct))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(pt) != original {
		t.Fatalf("Decrypt 应还原明文 %q, got %q", original, pt)
	}
	if n := mock.reqCount(); n != 1 {
		t.Fatalf("Decrypt 应恰好发 1 次请求, got %d", n)
	}
	if got := mock.reqToken(0); got != vaultTestToken {
		t.Fatalf("X-Vault-Token 应为 %q, got %q", vaultTestToken, got)
	}
	if got := mock.path(0); !strings.HasSuffix(got, "/v1/transit/decrypt/sproxy") {
		t.Fatalf("请求路径应为 /v1/transit/decrypt/sproxy 结尾, got %q", got)
	}
	body := mock.body(0)
	if body["ciphertext"] != ct {
		t.Fatalf("body.ciphertext 应为 %q, got %q", ct, body["ciphertext"])
	}
	if body["context"] != base64.StdEncoding.EncodeToString([]byte(vaultTestAAD)) {
		t.Fatalf("body.context 应为 base64(%q), got %q", vaultTestAAD, body["context"])
	}
}

// TestVaultTransitStorer_FullRoundtrip 验证 Encrypt→Decrypt 全链路往返：
// mock 默认行为下 Encrypt 密文内嵌 base64(明文)，Decrypt 解回原明文。
func TestVaultTransitStorer_FullRoundtrip(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	secret := []byte(`{"version":1,"keys":[{"ak":"ak-test-abc"}]}`)
	ct, err := s.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, secret) {
		t.Fatalf("全链路往返应还原明文, got %q", pt)
	}
}

// TestVaultTransitStorer_AADContext_Binding 验证 AAD context 绑定语义：
// 同 AADPath 的多次 Encrypt 发送一致且等于 base64(AADPath) 的 context 字段。
func TestVaultTransitStorer_AADContext_Binding(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	if _, err := s.Encrypt([]byte("first")); err != nil {
		t.Fatalf("Encrypt #1: %v", err)
	}
	if _, err := s.Encrypt([]byte("second")); err != nil {
		t.Fatalf("Encrypt #2: %v", err)
	}
	wantCtx := base64.StdEncoding.EncodeToString([]byte(vaultTestAAD))
	ctx0 := mock.body(0)["context"]
	ctx1 := mock.body(1)["context"]
	if ctx0 != wantCtx || ctx1 != wantCtx {
		t.Fatalf("两次 Encrypt 的 context 应均为 %q, got %q / %q", wantCtx, ctx0, ctx1)
	}
	if ctx0 != ctx1 {
		t.Fatalf("同 AADPath 两次 Encrypt 的 context 应一致, got %q vs %q", ctx0, ctx1)
	}
}

// TestVaultTransitStorer_AADContext_EmptyOmitsField 验证 aadPath 为空时请求体省略
// context 字段（Vault 允许缺省——无 AAD 绑定）。
func TestVaultTransitStorer_AADContext_EmptyOmitsField(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, "") // AADPath 空

	if _, err := s.Encrypt([]byte("no-aad")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, ok := mock.body(0)["context"]; ok {
		t.Fatalf("aadPath 为空时请求体不应含 context 字段, got %#v", mock.body(0))
	}
}

// TestVaultTransitStorer_Encrypt_CiphertextVerbatim 验证密文保存格式：
// Encrypt 返回的密文 = Vault 响应 data.ciphertext 原样（含版本字段，如 vault:v1:abc/def）。
func TestVaultTransitStorer_Encrypt_CiphertextVerbatim(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	const fixedCT = "vault:v1:abc/def/versioned"
	mock.setEncryptCiphertext(fixedCT)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	ct, err := s.Encrypt([]byte("anything"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(ct) != fixedCT {
		t.Fatalf("Encrypt 应原样返回 data.ciphertext %q, got %q", fixedCT, ct)
	}
}

// TestVaultTransitStorer_Decrypt_RejectNonVaultPrefix 验证非 vault:v1: 前缀输入直接拒绝：
// Decrypt 返回 error 且不发任何 Vault 请求（防明文误喂）。
func TestVaultTransitStorer_Decrypt_RejectNonVaultPrefix(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	_, err := s.Decrypt([]byte(`{"version":1,"keys":[]}`))
	if err == nil {
		t.Fatalf("Decrypt 非 vault:v1: 前缀输入应报错")
	}
	if n := mock.reqCount(); n != 0 {
		t.Fatalf("非 vault:v1: 输入不应请求 Vault, got %d 次请求", n)
	}
}

// TestVaultTransitStorer_New_Validation 验证构造函数 fail-fast 校验：
// addr 非空 + http/https scheme + host 非空、key_name 非空、token 非空（空 token →
// 「vault: token 为空」）。
func TestVaultTransitStorer_New_Validation(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	tests := []struct {
		name    string
		opts    VaultOptions
		wantErr string
	}{
		{
			name:    "空 addr",
			opts:    VaultOptions{KeyName: vaultTestKey, Token: vaultTestToken},
			wantErr: "addr",
		},
		{
			name:    "非法 scheme",
			opts:    VaultOptions{Addr: "ftp://vault:8200", KeyName: vaultTestKey, Token: vaultTestToken},
			wantErr: "scheme",
		},
		{
			name:    "缺 host",
			opts:    VaultOptions{Addr: "http://", KeyName: vaultTestKey, Token: vaultTestToken},
			wantErr: "host",
		},
		{
			name:    "空 key_name",
			opts:    VaultOptions{Addr: mock.URL(), Token: vaultTestToken},
			wantErr: "key_name",
		},
		{
			name:    "空 token",
			opts:    VaultOptions{Addr: mock.URL(), KeyName: vaultTestKey},
			wantErr: "token 为空",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewVaultTransitStorer(tc.opts)
			if err == nil {
				t.Fatalf("NewVaultTransitStorer 应返回错误（want substr %q）", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestVaultTransitStorer_New_DefaultMount 验证 mount 缺省为 "transit"（请求路径含
// /v1/transit/...）。
func TestVaultTransitStorer_New_DefaultMount(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s, err := NewVaultTransitStorer(VaultOptions{
		Addr: mock.URL(), KeyName: vaultTestKey, Token: vaultTestToken,
	})
	if err != nil {
		t.Fatalf("NewVaultTransitStorer: %v", err)
	}
	if _, err := s.Encrypt([]byte("x")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got := mock.path(0); !strings.HasSuffix(got, "/v1/transit/encrypt/sproxy") {
		t.Fatalf("mount 缺省应为 transit, 请求路径异常: %q", got)
	}
}

// TestVaultTransitStorer_New_CAFileValidation 验证 CAFile 加载分支：
// 文件不存在 / 内容非有效 PEM 均返回明确 error。
func TestVaultTransitStorer_New_CAFileValidation(t *testing.T) {
	dir := t.TempDir()
	base := VaultOptions{Addr: "https://vault:8200", KeyName: vaultTestKey, Token: vaultTestToken}

	t.Run("CA 文件不存在", func(t *testing.T) {
		opts := base
		opts.CAFile = filepath.Join(dir, "missing.pem")
		_, err := NewVaultTransitStorer(opts)
		if err == nil {
			t.Fatalf("CA 文件不存在时应返回错误")
		}
		if !strings.Contains(err.Error(), "CA 文件") {
			t.Fatalf("错误应含 CA 文件线索, got %v", err)
		}
	})
	t.Run("CA 内容非 PEM", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.pem")
		if err := os.WriteFile(bad, []byte("not-a-pem"), 0o600); err != nil {
			t.Fatalf("写临时文件: %v", err)
		}
		opts := base
		opts.CAFile = bad
		_, err := NewVaultTransitStorer(opts)
		if err == nil {
			t.Fatalf("CA 内容非 PEM 时应返回错误")
		}
		if !strings.Contains(err.Error(), "PEM") {
			t.Fatalf("错误应含 PEM 线索, got %v", err)
		}
	})
}

// TestVaultTransitStorer_ErrorClassification 验证错误分类（fail-closed）：
//   - 非 200 + {"errors":["permission denied"]} → error 含 "permission denied"（403 语义）；
//   - 503 + {"errors":["Vault is sealed"]} → error 含 "sealed"（明确 Vault sealed，需 unseal）；
//   - 5xx → error 含状态码；
//   - Vault 不可达（addr 指向已关闭端口）→ error 含 "vault"（网络）。
func TestVaultTransitStorer_ErrorClassification(t *testing.T) {
	t.Run("4xx 权限拒绝解析 Vault errors", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("encrypt", http.StatusForbidden, `{"errors":["permission denied"]}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		_, err := s.Encrypt([]byte("x"))
		if err == nil {
			t.Fatalf("403 响应 Encrypt 应报错")
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("错误应含 Vault 消息 permission denied, got %v", err)
		}
	})
	t.Run("404 key 不存在含 Vault 消息", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("decrypt", http.StatusNotFound, `{"errors":["key not found"]}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		_, err := s.Decrypt([]byte("vault:v1:abc"))
		if err == nil {
			t.Fatalf("404 响应 Decrypt 应报错")
		}
		if !strings.Contains(err.Error(), "key not found") {
			t.Fatalf("错误应含 Vault 消息 key not found, got %v", err)
		}
	})
	t.Run("503 sealed 特别标注", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("decrypt", http.StatusServiceUnavailable, `{"errors":["Vault is sealed"]}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		_, err := s.Decrypt([]byte("vault:v1:abc"))
		if err == nil {
			t.Fatalf("503 响应 Decrypt 应报错")
		}
		if !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("错误应含 sealed, got %v", err)
		}
		if !strings.Contains(err.Error(), "unseal") {
			t.Fatalf("503 sealed 错误应标注需 unseal, got %v", err)
		}
	})
	t.Run("5xx 含状态码", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("encrypt", http.StatusInternalServerError, `{"errors":["internal error"]}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		_, err := s.Encrypt([]byte("x"))
		if err == nil {
			t.Fatalf("500 响应 Encrypt 应报错")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Fatalf("5xx 错误应含状态码 500, got %v", err)
		}
	})
	t.Run("Vault 不可达为网络错误", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead.Close() // 关闭后端口不可达
		s, err := NewVaultTransitStorer(VaultOptions{
			Addr: dead.URL, KeyName: vaultTestKey, Token: vaultTestToken,
		})
		if err != nil {
			t.Fatalf("NewVaultTransitStorer: %v", err)
		}
		if _, err := s.Encrypt([]byte("x")); err == nil {
			t.Fatalf("Vault 不可达时 Encrypt 应报错")
		} else if !strings.Contains(err.Error(), "vault") || !strings.Contains(err.Error(), "不可达") ||
			!strings.Contains(err.Error(), "encrypt") {
			t.Fatalf("网络错误应为 vault: encrypt ...不可达 分类（区分 vaultAPIError）, got %v", err)
		}
	})
}

// TestVaultTransitStorer_Encrypt_NoFollowRedirect 验证 http.Client 禁止跟随重定向：
// mock 返回 302 + Location 指向另一 host，请求不被跟随（X-Vault-Token 不外泄到重定向
// 目标），302 响应原样收尾为错误（含状态码）。
func TestVaultTransitStorer_Encrypt_NoFollowRedirect(t *testing.T) {
	leakTarget := newMockVault(t, vaultTestToken) // 若被跟随将收到带 token 的请求
	mock := newMockVault(t, vaultTestToken)
	mock.overrideResp("encrypt", vaultMockResp{
		status: http.StatusFound,
		body:   `{"errors":["redirecting"]}`,
		headers: map[string]string{
			"Location": leakTarget.URL() + "/v1/transit/encrypt/sproxy",
		},
	})
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	_, err := s.Encrypt([]byte("secret"))
	if err == nil {
		t.Fatalf("302 响应 Encrypt 应报错（不跟随重定向）")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Fatalf("错误应含 302 状态码（原样收尾跳转响应）, got %v", err)
	}
	if n := leakTarget.reqCount(); n != 0 {
		t.Fatalf("重定向不应被跟随（target 收到 %d 次请求，X-Vault-Token 有外泄风险）", n)
	}
}

// TestVaultTransitStorer_CAFile_RealTLSCert 验证 CAFile 正路径：以 httptest.NewTLSServer
// 的自签证书为 CA 装配 storer，Encrypt/Decrypt 经 TLS 真连通（验证 RootCAs/Transport 克隆
// 装配，M-6）。
func TestVaultTransitStorer_CAFile_RealTLSCert(t *testing.T) {
	mock, caPath := newMockVaultTLS(t, vaultTestToken)
	s, err := NewVaultTransitStorer(VaultOptions{
		Addr: mock.URL(), Mount: vaultTestMount, KeyName: vaultTestKey,
		Token: vaultTestToken, AADPath: vaultTestAAD, CAFile: caPath,
	})
	if err != nil {
		t.Fatalf("NewVaultTransitStorer(CAFile): %v", err)
	}
	secret := []byte("over-tls-secret")
	ct, err := s.Encrypt(secret)
	if err != nil {
		t.Fatalf("CAFile Encrypt（TLS 真连通）: %v", err)
	}
	pt, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("CAFile Decrypt: %v", err)
	}
	if !bytes.Equal(pt, secret) {
		t.Fatalf("CAFile 全链路往返应还原明文, got %q", pt)
	}
}

// TestVaultTransitStorer_EmptyPlaintext_Roundtrip 验证空明文往返：base64("")=="" 是合法
// data.plaintext，响应字段判存在性（指针）不应把空串当缺字段拒绝（M-3）。
func TestVaultTransitStorer_EmptyPlaintext_Roundtrip(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newTestVaultStorer(t, mock, vaultTestAAD)

	ct, err := s.Encrypt(nil)
	if err != nil {
		t.Fatalf("Encrypt(空): %v", err)
	}
	pt, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt(空明文密文): %v", err)
	}
	if len(pt) != 0 {
		t.Fatalf("空明文往返应还原为空, got %q", pt)
	}
}

// TestVaultTransitStorer_SuccessResponse_GuardBranches 验证成功响应（200）的守卫分支：
// 缺字段 / 非 JSON / plaintext 非法 base64 均返回明确 error（fail-closed，不 panic）。
func TestVaultTransitStorer_SuccessResponse_GuardBranches(t *testing.T) {
	t.Run("Encrypt 缺 data.ciphertext", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("encrypt", http.StatusOK, `{"data":{}}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		if _, err := s.Encrypt([]byte("x")); err == nil || !strings.Contains(err.Error(), "data.ciphertext") {
			t.Fatalf("缺 data.ciphertext 应报错含字段名, got %v", err)
		}
	})
	t.Run("Decrypt 缺 data.plaintext", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("decrypt", http.StatusOK, `{"data":{}}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		if _, err := s.Decrypt([]byte("vault:v1:abc")); err == nil || !strings.Contains(err.Error(), "data.plaintext") {
			t.Fatalf("缺 data.plaintext 应报错含字段名, got %v", err)
		}
	})
	t.Run("200 非 JSON", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("encrypt", http.StatusOK, `not-json`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		if _, err := s.Encrypt([]byte("x")); err == nil || !strings.Contains(err.Error(), "解析") {
			t.Fatalf("200 非 JSON 应报解析错误, got %v", err)
		}
	})
	t.Run("data.plaintext 非法 base64", func(t *testing.T) {
		mock := newMockVault(t, vaultTestToken)
		mock.override("decrypt", http.StatusOK, `{"data":{"plaintext":"!!!not-base64!!!"}}`)
		s := newTestVaultStorer(t, mock, vaultTestAAD)

		if _, err := s.Decrypt([]byte("vault:v1:abc")); err == nil || !strings.Contains(err.Error(), "base64") {
			t.Fatalf("plaintext 非法 base64 应报错, got %v", err)
		}
	})
}

// ---- Task 2：decrypt 短 TTL 缓存 ----

// newCachedVaultStorer 用指定 CacheTTL 构造缓存开启的 VaultTransitStorer（失败即终止）。
func newCachedVaultStorer(t *testing.T, mock *mockVaultServer, ttl time.Duration, aadPath string) *VaultTransitStorer {
	t.Helper()
	s, err := NewVaultTransitStorer(VaultOptions{
		Addr:     mock.URL(),
		Mount:    vaultTestMount,
		KeyName:  vaultTestKey,
		Token:    vaultTestToken,
		AADPath:  aadPath,
		CacheTTL: ttl,
	})
	if err != nil {
		t.Fatalf("NewVaultTransitStorer(CacheTTL=%s): %v", ttl, err)
	}
	return s
}

// TestVaultCache_Hit_NoSecondRequest 验证缓存命中：CacheTTL>0 时同密文二次 Decrypt 命中
// 缓存，mock 只收到 1 次请求，两次结果 bytes 相等。
func TestVaultCache_Hit_NoSecondRequest(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	mock.setDecryptFn(func(ciphertext string) string { return "pt:" + ciphertext })
	s := newCachedVaultStorer(t, mock, time.Hour, vaultTestAAD)

	const ct = "vault:v1:same"
	first, err := s.Decrypt([]byte(ct))
	if err != nil {
		t.Fatalf("Decrypt #1: %v", err)
	}
	second, err := s.Decrypt([]byte(ct))
	if err != nil {
		t.Fatalf("Decrypt #2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("缓存命中两次结果应一致, got %q vs %q", first, second)
	}
	if n := mock.reqCount(); n != 1 {
		t.Fatalf("缓存命中后 mock 应只收到 1 次请求, got %d", n)
	}
}

// TestVaultCache_Hit_ReturnsCopy 验证命中返回的是副本：篡改返回明文不影响缓存内部 buffer，
// 二次 Decrypt 仍返回原始明文（且不触发第二次 Vault 请求）。
func TestVaultCache_Hit_ReturnsCopy(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	mock.setDecryptFn(func(string) string { return "sensitive-data" })
	s := newCachedVaultStorer(t, mock, time.Hour, vaultTestAAD)

	const ct = "vault:v1:copy"
	first, err := s.Decrypt([]byte(ct))
	if err != nil {
		t.Fatalf("Decrypt #1: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("测试前提不成立：明文为空无法验证副本隔离")
	}
	first[0] = 'X' // 篡改返回明文
	second, err := s.Decrypt([]byte(ct))
	if err != nil {
		t.Fatalf("Decrypt #2: %v", err)
	}
	if string(second) != "sensitive-data" {
		t.Fatalf("命中缓存应返回不受篡改影响的明文, got %q", second)
	}
	if n := mock.reqCount(); n != 1 {
		t.Fatalf("缓存命中后 mock 应只收到 1 次请求, got %d", n)
	}
}

// TestVaultCache_Expired_Refetch 验证缓存过期后重新请求 Vault。为确定性直接篡改缓存
// entry 的 expires 为过去（不走短 TTL + sleep 的时序依赖）。
func TestVaultCache_Expired_Refetch(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	mock.setDecryptFn(func(string) string { return "pt" })
	s := newCachedVaultStorer(t, mock, time.Hour, vaultTestAAD)

	const ct = "vault:v1:exp"
	if _, err := s.Decrypt([]byte(ct)); err != nil {
		t.Fatalf("Decrypt #1: %v", err)
	}
	key := string(ct)
	s.mu.Lock()
	e, ok := s.cache[key]
	if !ok {
		s.mu.Unlock()
		t.Fatalf("首次 Decrypt 后缓存应含该密文")
	}
	e.expires = time.Now().Add(-time.Second)
	s.cache[key] = e
	s.mu.Unlock()

	if _, err := s.Decrypt([]byte(ct)); err != nil {
		t.Fatalf("Decrypt #2: %v", err)
	}
	if n := mock.reqCount(); n != 2 {
		t.Fatalf("过期后应重新请求 Vault, mock 应收到 2 次, got %d", n)
	}
}

// TestVaultCache_DifferentCiphertext_NoShare 验证不同密文不共享缓存项（各自请求 Vault）。
func TestVaultCache_DifferentCiphertext_NoShare(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	mock.setDecryptFn(func(ciphertext string) string { return "pt:" + ciphertext })
	s := newCachedVaultStorer(t, mock, time.Hour, vaultTestAAD)

	ptA, err := s.Decrypt([]byte("vault:v1:A"))
	if err != nil {
		t.Fatalf("Decrypt A: %v", err)
	}
	ptB, err := s.Decrypt([]byte("vault:v1:B"))
	if err != nil {
		t.Fatalf("Decrypt B: %v", err)
	}
	if bytes.Equal(ptA, ptB) {
		t.Fatalf("不同密文应解出不同明文, got %q", ptA)
	}
	if n := mock.reqCount(); n != 2 {
		t.Fatalf("不同密文应各请求一次 Vault, mock 应收到 2 次, got %d", n)
	}
}

// TestVaultCache_DisabledWhenTTLZero 验证 CacheTTL=0（关闭）：缓存 map 为 nil，每次
// Decrypt 直查 Vault（mock 收 2 次）。
func TestVaultCache_DisabledWhenTTLZero(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	mock.setDecryptFn(func(string) string { return "pt" })
	s := newTestVaultStorer(t, mock, vaultTestAAD) // CacheTTL 未设 = 0
	if s.cache != nil {
		t.Fatalf("CacheTTL=0 时缓存 map 应为 nil")
	}

	const ct = "vault:v1:off"
	if _, err := s.Decrypt([]byte(ct)); err != nil {
		t.Fatalf("Decrypt #1: %v", err)
	}
	if _, err := s.Decrypt([]byte(ct)); err != nil {
		t.Fatalf("Decrypt #2: %v", err)
	}
	if n := mock.reqCount(); n != 2 {
		t.Fatalf("缓存关闭时每次应直查 Vault, mock 应收到 2 次, got %d", n)
	}
}

// TestVaultCache_Encrypt_DoesNotTouch 验证 Encrypt 不写缓存（只缓存 Decrypt 结果）。
func TestVaultCache_Encrypt_DoesNotTouch(t *testing.T) {
	mock := newMockVault(t, vaultTestToken)
	s := newCachedVaultStorer(t, mock, time.Hour, vaultTestAAD)

	if _, err := s.Encrypt([]byte("secret")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	s.mu.Lock()
	n := len(s.cache)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("Encrypt 不应写缓存, 缓存长度应为 0, got %d", n)
	}
}
