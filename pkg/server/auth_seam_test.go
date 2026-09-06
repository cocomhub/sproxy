// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// fakeAuthenticator 是测试用宿主 Authenticator：固定返回预设 Principal（不验签）。
// 用于验证 RegisterRoutesOpts.Authenticators 宿主注入（R3-I1/I2）与链语义（R4-I3）。
type fakeAuthenticator struct {
	name      string
	principal *Principal
	err       error
}

func (f *fakeAuthenticator) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, _ *http.Request) (*Principal, error) {
	return f.principal, f.err
}

// newAuthSeamServer 启动带可选自定义 Authenticator 链的完整路由测试服务器。
// optsMod 为 nil 时装配默认链（nil → RegisterRoutes 默认 [RingAuthenticator]）。
// 返回服务地址、cfgPtr 与存储根（供落桶断言）。
func newAuthSeamServer(t *testing.T, modifyCfg func(*Config), optsMod func(*RegisterRoutesOpts)) (string, *atomic.Pointer[Config], string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "test-version",
		BuildAt:     "test-buildat",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	}
	if optsMod != nil {
		optsMod(&opts)
	}
	h := RegisterRoutes(t.Context(), opts)

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, &cfgPtr, tmpDir
}

// ---- 默认链行为不变（4A 零回归）----

// TestAuthSeam_DefaultChain_BehavesAs4A 验证 RegisterRoutes（无 Authenticators 注入）
// 装配的默认链行为与 4A 一致：SproxySig 验签成功 → GET /api/files 200；操作审计 actor
// 正确（= 命中 AK）。
func TestAuthSeam_DefaultChain_BehavesAs4A(t *testing.T) {
	url, cfgPtr, auditBuf := newAuditTestServer(t, nil)

	// GET /api/files 带合法 SproxySig 签名 → 200（默认链 = RingAuthenticator）。
	listReq, _ := http.NewRequest("GET", url+"/api/files", nil)
	signRequest(listReq, testAccessKey, testAccessSecret)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/files status = %d, want 200", listResp.StatusCode)
	}

	// 删除操作审计 actor 正确（ActorFrom(ctx) = Principal.AK = testAccessKey）。
	writeUploadFile(t, cfgPtr, "seam-default.txt", []byte("x"))
	delReq, _ := http.NewRequest("POST", url+"/delete?filename=seam-default.txt", nil)
	delReq.Header.Set("X-File-Checksum", sha256hex([]byte("x")))
	signRequest(delReq, testAccessKey, testAccessSecret)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("POST /delete: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /delete status = %d, want 200", delResp.StatusCode)
	}
	m := findAudit(t, auditBuf, "delete")
	if got, _ := m["actor"].(string); got != testAccessKey {
		t.Fatalf("delete 审计 actor = %q, want %q（默认链认证后 withActor 写 Principal.AK）", got, testAccessKey)
	}
}

// ---- 宿主注入自定义 Authenticator（R3-I1）+ replace 语义 ----

// TestAuthSeam_HostAuthenticator_PrincipalInCtx 白盒验证宿主 Authenticator 的
// Principal 写入 ctx：PrincipalFrom(ctx) 可读 Owner 元数据；ActorFrom 统一写
// Principal.AK（R4-M1）。
func TestAuthSeam_HostAuthenticator_PrincipalInCtx(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr}
	h.authenticators = []Authenticator{
		&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Owner: "ext-display-name", Role: string(accesskey.RoleUser)}},
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if p == nil {
			t.Error("PrincipalFrom(ctx) = nil")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if p.Owner != "ext-display-name" {
			t.Errorf("Principal.Owner = %q, want ext-display-name", p.Owner)
		}
		if got := ActorFrom(r.Context()); got != "ext-user-1" {
			t.Errorf("ActorFrom = %q, want ext-user-1（withActor 统一写 Principal.AK，R4-M1）", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/api/files", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// TestAuthSeam_HostAuthenticator_ReplaceChain_BucketByAK 验证宿主注入不含
// RingAuthenticator 的链（replace 默认链）：无 Authorization 头请求直接由 fake 认证
// 成功；文件操作按 Principal.AK 落桶（Owner 不参与落桶）。
func TestAuthSeam_HostAuthenticator_ReplaceChain_BucketByAK(t *testing.T) {
	url, _, storageRoot := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		// replace 默认链：只含 fake（不含 RingAuthenticator）。
		opts.Authenticators = []Authenticator{
			&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Owner: "ext-display-name", Role: string(accesskey.RoleUser)}},
		}
	})

	// 无 Authorization 头 → 链尾 fake 直接认证成功 → 上传 200。
	body := []byte("bucket-content")
	status, respBody := uploadFile(t, url, "bucket-check.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})
	if status != http.StatusOK {
		t.Fatalf("upload status = %d (body=%s)", status, respBody)
	}

	// 断言按 Principal.AK="ext-user-1" 落桶（Owner="ext-display-name" 不参与落桶）。
	full := filepath.Join(storageRoot, "ext-user-1", "user", "bucket-check.txt")
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("文件未落在 ext-user-1 桶（%s）: %v", full, err)
	}
	// Owner 桶不应被创建。
	if _, err := os.Stat(filepath.Join(storageRoot, "ext-display-name")); err == nil {
		t.Fatalf("Principal.Owner 不应参与落桶（ext-display-name 桶不应存在）")
	}

	// GET /api/files 列出 ext-user-1 桶内容（按 AK 落桶零回归）。
	listReq, _ := http.NewRequest("GET", url+"/api/files", nil)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/files status = %d, want 200", listResp.StatusCode)
	}
	var lr listResponse
	if err := json.NewDecoder(listResp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	found := false
	for _, f := range lr.Files {
		if f.Name == "bucket-check.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ext-user-1 桶列表未包含 bucket-check.txt，实际：%+v", lr.Files)
	}
}

// ---- 零凭据窗口宿主认证（R3-I2）----

// TestAuthSeam_ZeroCredentialWindow_HostAuthenticator 验证 ring 空（零凭据启动状态）
// 时注入宿主 Authenticator → 文件列表 200：链先跑，handleNoCredentials 兜底不前置拦截。
func TestAuthSeam_ZeroCredentialWindow_HostAuthenticator(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		// 零凭据窗口：空 Ring（无任何凭据）+ 宿主 Authenticator。
		noAuth := defaultNoAuthRegOpts()
		opts.CredentialRing = noAuth.CredentialRing
		opts.CredentialStore = nil
		opts.Authenticators = []Authenticator{
			&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: string(accesskey.RoleUser)}},
		}
	})

	// 未带 Authorization 头请求由宿主 fake 认证成功（链先跑；兜底不拦截）。
	req, _ := http.NewRequest("GET", url+"/api/files", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/files status = %d, want 200（零凭据窗口宿主认证）", resp.StatusCode)
	}
}

// ---- requireRole 门禁 ----

// TestAuthSeam_RequireRole_FileGroupGate 验证文件组 requireRole 门禁：fake 返回
// Role:"node" → 403；"user"/"admin" → 200；"" → 归一 user 放行（R3-M4）。
func TestAuthSeam_RequireRole_FileGroupGate(t *testing.T) {
	cases := []struct {
		name string
		role string
		want int
	}{
		{"node 角色被文件组拒绝", string(accesskey.RoleNode), http.StatusForbidden},
		{"user 角色放行", string(accesskey.RoleUser), http.StatusOK},
		{"admin 角色放行", string(accesskey.RoleAdmin), http.StatusOK},
		{"空 Role 归一 user 放行", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
				opts.Authenticators = []Authenticator{
					&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: tc.role}},
				}
			})
			req, _ := http.NewRequest("GET", url+"/api/files", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /api/files: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("Role=%q GET /api/files status = %d, want %d", tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestRequireRole_Matrix 是 requireRole 直接单元测试（顺序/空值归一/401/403 语义）。
func TestRequireRole_Matrix(t *testing.T) {
	tests := []struct {
		name      string
		principal *Principal
		minRole   string
		want      error
	}{
		{"nil principal → 401", nil, string(accesskey.RoleUser), errUnauthorized},
		{"user 组放行 user", &Principal{Role: string(accesskey.RoleUser)}, string(accesskey.RoleUser), nil},
		{"user 组放行 admin", &Principal{Role: string(accesskey.RoleAdmin)}, string(accesskey.RoleUser), nil},
		{"user 组拒绝 node", &Principal{Role: string(accesskey.RoleNode)}, string(accesskey.RoleUser), errForbidden},
		{"user 组空 Role 归一 user 放行", &Principal{Role: ""}, string(accesskey.RoleUser), nil},
		{"node 组放行 node", &Principal{Role: string(accesskey.RoleNode)}, string(accesskey.RoleNode), nil},
		{"node 组放行 admin", &Principal{Role: string(accesskey.RoleAdmin)}, string(accesskey.RoleNode), nil},
		{"node 组拒绝 user", &Principal{Role: string(accesskey.RoleUser)}, string(accesskey.RoleNode), errForbidden},
		{"admin 组仅放行 admin", &Principal{Role: string(accesskey.RoleAdmin)}, string(accesskey.RoleAdmin), nil},
		{"admin 组拒绝 user", &Principal{Role: string(accesskey.RoleUser)}, string(accesskey.RoleAdmin), errForbidden},
		{"admin 组拒绝 node", &Principal{Role: string(accesskey.RoleNode)}, string(accesskey.RoleAdmin), errForbidden},
		{"未知角色按最低档（user 组拒绝）", &Principal{Role: "superuser"}, string(accesskey.RoleUser), errForbidden},
		{"未知 minRole fail-closed", &Principal{Role: string(accesskey.RoleAdmin)}, "owener", errForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requireRole(tt.principal, tt.minRole)
			if got != tt.want {
				t.Fatalf("requireRole(%+v, %q) = %v, want %v", tt.principal, tt.minRole, got, tt.want)
			}
		})
	}
}

// ---- 回环直通合成 Principal（R4-I2）----

// TestAuthSeam_LoopbackPassThrough_HTTP 验证零凭据窗口（ring 空 + 无宿主
// Authenticator）+ allow_insecure_loopback=true + 回环来源 → GET /api/files 200
// （handleNoCredentials 回环直通合成最小 Principal，requireRole 放行）。
func TestAuthSeam_LoopbackPassThrough_HTTP(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		noAuth := defaultNoAuthRegOpts()
		opts.CredentialRing = noAuth.CredentialRing
		opts.CredentialStore = noAuth.CredentialStore
		opts.AllowInsecureLoopback = noAuth.AllowInsecureLoopback
	})

	resp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("零凭据窗口回环直通 GET /api/files status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthSeam_NoCredentials_NonLoopback401 验证零凭据窗口 + 非回环来源 → 401。
func TestAuthSeam_NoCredentials_NonLoopback401(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, credentialRing: emptyTestRing(), allowInsecureLoopback: true}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/api/files", nil)
	r.RemoteAddr = "192.168.1.5:9999" // 非回环来源
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("非回环来源 status = %d, want 401", w.Code)
	}
	if called {
		t.Fatal("非回环来源不应放行")
	}
}

// TestAuthSeam_LoopbackPassThrough_SynthesizesPrincipal 白盒验证 handleNoCredentials
// 回环直通分支合成最小 Principal{AK:"", Owner:"", Role:"user"} 写入 ctx（R4-I2）。
func TestAuthSeam_LoopbackPassThrough_SynthesizesPrincipal(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, allowInsecureLoopback: true}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		p := PrincipalFrom(r.Context())
		if p == nil {
			t.Error("PrincipalFrom(ctx) = nil，回环直通应合成 Principal")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if p.Role != string(accesskey.RoleUser) {
			t.Errorf("合成 Principal.Role = %q, want user", p.Role)
		}
		if p.AK != "" || p.Owner != "" {
			t.Errorf("合成 Principal 应为最小形态（AK/Owner 空），got AK=%q Owner=%q", p.AK, p.Owner)
		}
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest("GET", "/api/files", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.handleNoCredentials(w, r, cfgPtr.Load(), inner)

	if !called {
		t.Fatal("回环直通应放行 next")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// ---- /tunnel 链认证解密正常（R5-I1）----

// TestAuthSeam_TunnelChainDerivesKey 验证 SproxySig 验签成功（RingAuthenticator 填充
// Principal.Secret）→ POST /tunnel → 服务端用 PrincipalFrom(ctx).Secret 派生隧道密钥
// → 隧道请求解密正常（复用 4A 隧道 E2E 加密帧路径）。
func TestAuthSeam_TunnelChainDerivesKey(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts) // 默认链：RingAuthenticator 填充 Secret
	})

	key, err := tunnel.DeriveTunnelKey(testAccessSecret, "")
	if err != nil {
		t.Fatalf("DeriveTunnelKey: %v", err)
	}
	tc, err := tunnel.NewClient(hex.EncodeToString(key), url+"/tunnel", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("tunnel.NewClient: %v", err)
	}
	base := tc.HTTPClient.Transport
	tc.HTTPClient.Transport = &tunnelSignTransport{base: base, ak: testAccessKey, sk: testAccessSecret}

	req, _ := http.NewRequest("GET", "/api/files", nil)
	resp, err := tc.Do(req)
	if err != nil {
		t.Fatalf("tunnel Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel GET /api/files status = %d, want 200", resp.StatusCode)
	}
}

// ---- 宿主 Authenticator /tunnel 不派生（R5-I1）----

// TestAuthSeam_HostAuthenticator_TunnelNoDerivation 验证宿主 fake 认证（Principal.Secret
// 为空）→ POST /tunnel → 不派生隧道密钥（401：无 SproxySig 凭据不建立隧道）。
func TestAuthSeam_HostAuthenticator_TunnelNoDerivation(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		opts.Authenticators = []Authenticator{
			&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: string(accesskey.RoleUser)}},
		}
	})

	req, _ := http.NewRequest("POST", url+"/tunnel", strings.NewReader("raw"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /tunnel: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("宿主 Authenticator（无 Secret）POST /tunnel status = %d, want 401（不派生隧道密钥）", resp.StatusCode)
	}
}

// ---- api_keys 优先 ----

// TestAuthSeam_APIKeysTakePrecedence 验证 APIKeys.Enabled=true 时 Bearer 路径仍走既有
// 逻辑（Authenticator 链不生效）——即使宿主链会认证一切，无 Bearer 头仍 401。
func TestAuthSeam_APIKeysTakePrecedence(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, func(cfg *Config) {
		cfg.APIKeys = APIKeyConfig{
			Enabled: true,
			Keys:    []APIKey{{Name: "ops", Key: "mykey", Permission: "write"}},
		}
	}, func(opts *RegisterRoutesOpts) {
		// 注入会「认证一切」的宿主链——api_keys 启用时链不生效（前置分支优先）。
		opts.Authenticators = []Authenticator{
			&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: string(accesskey.RoleUser)}},
		}
	})

	// Bearer 请求走 api_keys 路径（actor=ops）→ 200。
	okReq, _ := http.NewRequest("GET", url+"/api/files", nil)
	okReq.Header.Set("Authorization", "Bearer mykey")
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("GET /api/files (Bearer): %v", err)
	}
	_ = okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer 请求 status = %d, want 200", okResp.StatusCode)
	}

	// 无 Bearer 头请求即使 fake 链会认证也不放行（api_keys 前置分支优先）。
	noAuthReq, _ := http.NewRequest("GET", url+"/api/files", nil)
	noAuthResp, err := http.DefaultClient.Do(noAuthReq)
	if err != nil {
		t.Fatalf("GET /api/files (no auth): %v", err)
	}
	_ = noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 Bearer 头 status = %d, want 401（api_keys 优先，链不生效）", noAuthResp.StatusCode)
	}
}

// ---- 链中失败继续尝试（R4-I3）----

// TestAuthSeam_ChainFailureContinuesToNext 验证注入 [RingAuthenticator, fakeAuth] 链，
// 请求无 Authorization 头 → 链首 RingAuthenticator 失败返回 error（不写响应）→ 链尾
// fake 认证成功 → 200（未被前置 401 短路）。
func TestAuthSeam_ChainFailureContinuesToNext(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts)
		opts.Authenticators = []Authenticator{
			NewRingAuthenticator(opts.CredentialRing, sproxysig.NewNoncePool()),
			&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: string(accesskey.RoleUser)}},
		}
	})

	// 无 Authorization 头：链首 RingAuthenticator 失败（不写响应）→ 链尾 fake 成功 → 200。
	req, _ := http.NewRequest("GET", url+"/api/files", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("链首失败后续尝试 GET /api/files status = %d, want 200", resp.StatusCode)
	}
}

// TestAuthSeam_HostAuthenticator_NilPrincipalContinue 验证宿主 Authenticator 违反接口
// 契约返回 (nil, nil) 时链内跳过该成员（不 panic 不解引用），后续成员仍可认证成功
// （与 R4-I3 链失败继续语义一致）。
func TestAuthSeam_HostAuthenticator_NilPrincipalContinue(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr}
	h.authenticators = []Authenticator{
		&fakeAuthenticator{principal: nil, err: nil}, // 违反契约：(nil, nil)
		&fakeAuthenticator{principal: &Principal{AK: "ext-user-1", Role: string(accesskey.RoleUser)}},
	}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/api/files", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（nil principal 成员被跳过，后续成员认证成功）", w.Code)
	}
	if !called {
		t.Fatal("后续成员认证成功后应放行 next")
	}
}

// ---- 显式空链 replace 语义（Minor 2 修复）----

// TestAuthSeam_EmptyChain_RespectedReplace 验证显式注入空链（[]Authenticator{}）不被
// 默认链覆盖：空链 = 无任何 authenticator → 所有请求未认证，走 handleNoCredentials
// 兜底（ring 空 + allow_insecure_loopback → 回环直通 200）。
func TestAuthSeam_EmptyChain_RespectedReplace(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		noAuth := defaultNoAuthRegOpts()
		opts.CredentialRing = noAuth.CredentialRing // 空 Ring
		opts.CredentialStore = nil
		opts.AllowInsecureLoopback = noAuth.AllowInsecureLoopback
		opts.Authenticators = []Authenticator{} // 显式空链（非 nil）→ replace 为空
	})

	// 回环来源（httptest 恒回环）→ handleNoCredentials 兜底直通 → 200。
	resp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("空链 + 回环 GET /api/files status = %d, want 200（handleNoCredentials 兜底）", resp.StatusCode)
	}
}

// TestAuthSeam_EmptyChain_NoAuthEvenWithSignature 是空链语义的判别性测试：即使带合法
// SproxySig 签名（默认链 RingAuthenticator 会认证成功 → 200），空链下也无任何
// authenticator → 未认证（ring 非空 → 401）。此用例在修复前（空链被静默回退默认链）
// 会返回 200，修复后 401。
func TestAuthSeam_EmptyChain_NoAuthEvenWithSignature(t *testing.T) {
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts)                     // 非空 Ring：testAccessKey
		opts.Authenticators = []Authenticator{} // 显式空链（非 nil）→ replace 默认链为空
	})

	req, _ := http.NewRequest("GET", url+"/api/files", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("空链 + 合法签名 GET /api/files status = %d, want 401（空链无任何 authenticator，不被默认链覆盖）", resp.StatusCode)
	}
}

// TestAuthSeam_EmptyChain_NonLoopback401 白盒验证显式空链 + 非回环来源 → 401。
func TestAuthSeam_EmptyChain_NonLoopback401(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, credentialRing: emptyTestRing()}
	h.authenticators = []Authenticator{} // 显式空链
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/api/files", nil)
	r.RemoteAddr = "192.168.1.5:9999" // 非回环来源
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("空链 + 非回环 status = %d, want 401", w.Code)
	}
	if called {
		t.Fatal("空链 + 非回环不应放行")
	}
}

// ---- RingAuthenticator Principal 映射（空 Role 归一 + Secret 填充）----

// TestAuthSeam_RingAuthenticator_PrincipalMapping 验证 RingAuthenticator 成功路径：
// Principal.Secret 填充命中条目 SK（R5-I1）；Key.Role 空值（UpsertAK 直建）归一 user。
func TestAuthSeam_RingAuthenticator_PrincipalMapping(t *testing.T) {
	ring := ringForTestCreds() // UpsertAK(ak, "test") 直建，Role 为空
	a := NewRingAuthenticator(ring, sproxysig.NewNoncePool())

	r := httptest.NewRequest("GET", "/api/files", nil)
	signRequest(r, testAccessKey, testAccessSecret)

	p, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("RingAuthenticator.Authenticate: %v", err)
	}
	if p.AK != testAccessKey {
		t.Errorf("Principal.AK = %q, want %q", p.AK, testAccessKey)
	}
	if p.Role != string(accesskey.RoleUser) {
		t.Errorf("空 Role 归一 Principal.Role = %q, want user", p.Role)
	}
	if len(p.Secret) == 0 {
		t.Error("Principal.Secret 应为空，RingAuthenticator 应填充命中条目 SK（R5-I1）")
	}
	if hex.EncodeToString(p.Secret) != testAccessSecret {
		t.Errorf("Principal.Secret 与 SK 不一致：%s", hex.EncodeToString(p.Secret))
	}
	if p.Owner != "test" {
		t.Errorf("Principal.Owner = %q, want test（ringForTestCreds UpsertAK owner）", p.Owner)
	}
}

// TestAuthSeam_RingAuthenticator_NoAuthHeaderError 验证 RingAuthenticator 对无
// Authorization 头返回 error（不写响应，R4-I3）。
func TestAuthSeam_RingAuthenticator_NoAuthHeaderError(t *testing.T) {
	a := NewRingAuthenticator(ringForTestCreds(), sproxysig.NewNoncePool())
	r := httptest.NewRequest("GET", "/api/files", nil)
	p, err := a.Authenticate(context.Background(), r)
	if err == nil {
		t.Fatal("无 Authorization 头应返回 error")
	}
	if p != nil {
		t.Fatalf("失败时 Principal 应为 nil，got %+v", p)
	}
}

// ---- RingAuthenticator 直建 Ring（GetKey Role 归一路径）----

// TestAuthSeam_RingAuthenticator_GetKeyEmptyRole 验证 RingAuthenticator 经 GetKey 读
// 空 Role 归一 user（R3-M4，含 UpsertAK 直建的空 Role key）。
func TestAuthSeam_RingAuthenticator_GetKeyEmptyRole(t *testing.T) {
	ak, sk, err := accesskey.GeneratePair(nil, "")
	if err != nil {
		t.Fatalf("GeneratePair: %v", err)
	}
	ring := accesskey.NewRing()
	_ = ring.UpsertAK(ak, "owner") // 直建：Role 为空
	skBytes, _ := hex.DecodeString(sk)
	id := testEntryID(ak)
	_, _ = ring.AddKey(ak, skBytes, accesskey.WithID(id))

	a := NewRingAuthenticator(ring, sproxysig.NewNoncePool())
	r := httptest.NewRequest("GET", "/api/files", nil)
	signRequest(r, ak, sk)
	p, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Role != string(accesskey.RoleUser) {
		t.Fatalf("UpsertAK 直建空 Role 归一 Principal.Role = %q, want user", p.Role)
	}
}

// 编译期断言：fakeAuthenticator 与 RingAuthenticator 实现 Authenticator。
var (
	_ Authenticator = (*fakeAuthenticator)(nil)
	_ Authenticator = (*RingAuthenticator)(nil)
)
