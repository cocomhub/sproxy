// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// 测试辅助常量：admin 凭据（testAdminKey 带 Meta.Type=="admin" 条目）。
const (
	testAdminKey    = "sk-admin-mesh-bbccdd"
	testAdminSecret = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// doSignedJSON 发一个带 SproxySig 签名的 JSON 请求（body 为 nil 时无 body 签名）。
func doSignedJSON(t *testing.T, method, url string, ak, sk string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		signBodyRequest(req, ak, sk, buf.Bytes())
	} else {
		signRequest(req, ak, sk)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func decodeJSONInto(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("解析 JSON 失败: %v (body=%s)", err, data)
	}
}

func signedGet(t *testing.T, url, ak, sk string) (int, []byte) {
	t.Helper()
	return doSignedJSON(t, http.MethodGet, url, ak, sk, nil)
}

func signedDelete(t *testing.T, url, ak, sk string) (int, []byte) {
	t.Helper()
	return doSignedJSON(t, http.MethodDelete, url, ak, sk, nil)
}

// testWrapKey 复算调用方侧 wrap 信封密钥（与生产 credentialWrapKey 同一拼法：
// context = credentialWrapContext[#mesh]）。mesh 传调用方 AK 派生值（测试 AK 为
// sk-test-mesh-... 时非空——覆盖 mesh 纳入派生）。
func testWrapKey(t *testing.T, sk []byte, ak, mesh string) []byte {
	t.Helper()
	if mesh == "" {
		mesh = accesskey.ParseMesh(ak)
	}
	ctx := credentialWrapContext
	if mesh != "" {
		ctx = credentialWrapContext + "#" + mesh
	}
	k, err := accesskey.DeriveWrapKey(sk, ak, ctx)
	if err != nil {
		t.Fatalf("DeriveWrapKey: %v", err)
	}
	return k
}

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	return b
}

// credentialsRingWithAdmin 构造带 admin 条目（Meta.Type=="admin"）与 user 条目的 Ring。
// adminAK 为空时只注入 user（无 admin 条目，模拟 4A 无 admin 部署）。
func credentialsRingWithAdmin(adminAK, adminSK, userAK, userSK string) *accesskey.Ring {
	ring := accesskey.NewRing()
	if adminAK != "" {
		ask, _ := hex.DecodeString(adminSK)
		_ = ring.UpsertAK(adminAK, "admin")
		_, _ = ring.AddKey(adminAK, ask, accesskey.WithMeta(accesskey.Meta{Type: "admin"}))
	}
	usk, _ := hex.DecodeString(userSK)
	if userAK != "" {
		_ = ring.UpsertAK(userAK, "user")
		_, _ = ring.AddKey(userAK, usk, accesskey.WithMeta(accesskey.Meta{Type: "initial"}))
	}
	return ring
}

// newCredentialsTestServer 启动带管理凭据的完整路由测试服务器。
// adminAK 为空 → 只注入 user（无 admin 条目）；auditBuf 非 nil → 捕获审计日志。
// storeOverride 非 nil → 替换注入的 CredentialStore（测试可注入必失败 store 验证
// 持久化失败路径）。
func newCredentialsTestServer(t *testing.T, adminAK, adminSK, userAK, userSK string, auditBuf *bytes.Buffer, storeOverride *CredentialStore) (string, *atomic.Pointer[Config], *accesskey.Ring) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	ring := credentialsRingWithAdmin(adminAK, adminSK, userAK, userSK)
	opts := RegisterRoutesOpts{
		Mux:             http.NewServeMux(),
		CfgPtr:          &cfgPtr,
		Version:         "test-version",
		BuildAt:         "test-buildat",
		Logger:          testLogger(),
		CredentialRing:  ring,
		CredentialStore: storeOverride,
	}
	if auditBuf != nil {
		opts.AuditLogger = slog.New(slog.NewJSONHandler(auditBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	} else {
		opts.AuditLogger = testLogger()
	}

	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, &cfgPtr, ring
}

// auditActions 扫描审计 buffer 中指定 action 的所有事件（返回 detail）。
func auditActions(t *testing.T, buf *bytes.Buffer, action string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("解析审计行失败 %q: %v", line, err)
		}
		if m["action"] == action {
			out = append(out, m)
		}
	}
	return out
}

// ---- renew ----

// TestCredentials_Renew_Roundtrip 验证 renew 全链路：返回结构、旧 SK 解 wrap 得新 SK、
// 新 SK 立即可用、旧 SK 仍可用（多 SK 共存）。
func TestCredentials_Renew_Roundtrip(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("renew status = %d, want 200 (body=%s)", status, body)
	}
	var resp renewCredentialResponse
	decodeJSONInto(t, body, &resp)
	if resp.AK != testAccessKey {
		t.Errorf("resp.ak = %q, want %q", resp.AK, testAccessKey)
	}
	if resp.SKID == "" {
		t.Errorf("resp.sk_id 为空")
	}
	if resp.Kind != accesskey.KindSecretWrap {
		t.Errorf("resp.kind = %q, want %q", resp.Kind, accesskey.KindSecretWrap)
	}
	if resp.WrapKeyAK != testAccessKey {
		t.Errorf("resp.wrap_key_ak = %q, want %q", resp.WrapKeyAK, testAccessKey)
	}
	if resp.WrappedSecret == nil || len(resp.WrappedSecret.Cipher) == 0 || len(resp.WrappedSecret.Nonce) == 0 {
		t.Fatalf("resp.wrapped_secret 结构不完整: %+v", resp.WrappedSecret)
	}
	if resp.WrappedSecret.Kind != accesskey.KindSecretWrap {
		t.Errorf("wrapped_secret.kind = %q, want secret_wrap", resp.WrappedSecret.Kind)
	}
	if resp.WrappedSecret.WrapKeyID != testAccessKey {
		t.Errorf("wrapped_secret.wrap_key_id = %q, want %q", resp.WrappedSecret.WrapKeyID, testAccessKey)
	}

	// 用旧 SK 解 wrap（调用方侧派生信封密钥）得新 SK。
	oldSK := mustDecodeHex(t, testAccessSecret)
	wkey := testWrapKey(t, oldSK, testAccessKey, "")
	newSK, err := accesskey.DecryptSecret(resp.WrappedSecret, wkey)
	if err != nil {
		t.Fatalf("用旧 SK 解 wrap 失败: %v", err)
	}
	if len(newSK) != 32 {
		t.Fatalf("新 SK 长度 = %d, want 32", len(newSK))
	}
	newSKHex := hex.EncodeToString(newSK)

	// 新 SK 立即可用。
	st, b := signedGet(t, url+"/api/stats", testAccessKey, newSKHex)
	if st != http.StatusOK {
		t.Fatalf("新 SK 访问 status = %d, want 200 (body=%s)", st, b)
	}
	// 旧 SK 仍可用。
	st2, _ := signedGet(t, url+"/api/stats", testAccessKey, testAccessSecret)
	if st2 != http.StatusOK {
		t.Fatalf("旧 SK 访问 status = %d, want 200", st2)
	}
}

// TestCredentials_Renew_TTLServer 是上述用例的完整实现（分开写便于通过 modifyCfg）。
func TestCredentials_Renew_TTLServer(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.CredentialTTL = 48 * time.Hour
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	ring := credentialsRingWithAdmin("", "", testAccessKey, testAccessSecret)
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "v",
		BuildAt:        "b",
		Logger:         testLogger(),
		AuditLogger:    testLogger(),
		CredentialRing: ring,
	}
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })
	url := ts.URL

	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{
		"ttl": "10s",
	})
	if status != http.StatusOK {
		t.Fatalf("renew status = %d, want 200 (body=%s)", status, body)
	}
	var resp renewCredentialResponse
	decodeJSONInto(t, body, &resp)
	diff := time.Until(resp.ExpiresAt)
	if diff < 47*time.Hour || diff > 49*time.Hour {
		t.Errorf("expires_at 应在 now+48h±1h 内，实际 diff=%v (expires_at=%v)", diff, resp.ExpiresAt)
	}
}

// ---- sk 列表 ----

// TestCredentials_SKList_OwnListing 验证本人列表含 renew 新条目（sk_id/expires 元数据）。
func TestCredentials_SKList_OwnListing(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, "", testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("renew status = %d", status)
	}
	var renewResp renewCredentialResponse
	decodeJSONInto(t, body, &renewResp)

	st, data := signedGet(t, url+"/api/credentials/"+testAccessKey+"/sk", testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("sk list status = %d, want 200 (body=%s)", st, data)
	}
	var list skListResponse
	decodeJSONInto(t, data, &list)
	if list.Total < 2 {
		t.Fatalf("sk list total = %d, want >= 2（initial + renew）", list.Total)
	}
	found := false
	for _, s := range list.SKs {
		if s.WrappedSecret == nil {
			t.Errorf("条目 %s wrapped_secret 缺失", s.SKID)
		}
		if s.SKID == renewResp.SKID {
			found = true
			if s.Status != accesskey.StatusActive {
				t.Errorf("新条目 status = %q, want active", s.Status)
			}
			if s.MetaType != "renew" {
				t.Errorf("新条目 meta_type = %q, want renew", s.MetaType)
			}
			if s.Expires.IsZero() {
				t.Errorf("新条目 expires 为空")
			}
		}
	}
	if !found {
		t.Fatalf("列表未包含 renew 新条目 %q: %+v", renewResp.SKID, list.SKs)
	}
}

// TestCredentials_SKList_WrapKeyIsolation 验证非本人条目的 wrapped_secret 用调用方
// SK 解不开（按 key 隔离）：admin 查看 user 列表，每条 wrapped_secret 用 user 自己 SK
// 派生密钥可解、用 admin SK 派生密钥解不开。
func TestCredentials_SKList_WrapKeyIsolation(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	st, data := signedGet(t, url+"/api/credentials/"+testAccessKey+"/sk", testAdminKey, testAdminSecret)
	if st != http.StatusOK {
		t.Fatalf("admin sk list status = %d, want 200 (body=%s)", st, data)
	}
	var list skListResponse
	decodeJSONInto(t, data, &list)
	if list.Total < 1 {
		t.Fatalf("admin 视角 user 列表 total = %d, want >= 1", list.Total)
	}

	userSK := mustDecodeHex(t, testAccessSecret)
	adminSK := mustDecodeHex(t, testAdminSecret)
	for _, s := range list.SKs {
		if s.WrappedSecret == nil {
			continue
		}
		// 调用方（admin）SK 派生密钥 → 应解不开（按 key 隔离）。
		adminWK := testWrapKey(t, adminSK, testAccessKey, "")
		if _, derr := accesskey.DecryptSecret(s.WrappedSecret, adminWK); derr == nil {
			t.Errorf("非本人条目用 admin SK 应解不开（隔离被破坏）: %s", s.SKID)
		}
		// 条目所有者（user）SK 派生密钥 → 应能解开。
		userWK := testWrapKey(t, userSK, testAccessKey, "")
		if _, err := accesskey.DecryptSecret(s.WrappedSecret, userWK); err != nil {
			t.Errorf("本人条目用 user SK 应能解开: %s err=%v", s.SKID, err)
		}
	}
}

// TestCredentials_SKList_ForbiddenForNonOwner 验证非本人非 admin 查看 SK 列表 → 404，
// 且不泄漏目标 AK 的条目数据。
func TestCredentials_SKList_ForbiddenForNonOwner(t *testing.T) {
	akA, skA := testAccessKey, testAccessSecret
	akB, skB := "sk-test-b-12eeb123", strings.Repeat("33", 32)
	url, _, ring := newCredentialsTestServer(t, "", "", akA, skA, nil, nil)
	// B 的凭据必须注入服务端 Ring（与认证共享同一实例）：newCredentialsTestServer
	// 默认只注入 admin+user(A)，否则 B 的签名请求在 authMiddleware 就 401，到不了
	// handler 的 404 分支（测试 setup 缺陷，非产品缺陷）。
	if err := ring.UpsertAK(akB, "user"); err != nil {
		t.Fatalf("UpsertAK(%q): %v", akB, err)
	}
	if _, err := ring.AddKey(akB, mustDecodeHex(t, skB), accesskey.WithMeta(accesskey.Meta{Type: "initial"})); err != nil {
		t.Fatalf("AddKey(akB): %v", err)
	}

	// 用户 B 用自己 AK 签名查用户 A 的 sk 列表 → 404（非本人非 admin，不泄漏存在性）。
	st, body := signedGet(t, url+"/api/credentials/"+akA+"/sk", akB, skB)
	if st != http.StatusNotFound {
		t.Fatalf("B 查 A 的 sk 列表 status = %d, want 404 (body=%s)", st, body)
	}
	// 404 响应不得含 A 的条目数据（不泄漏）。
	if bytes.Contains(body, []byte("sk_id")) {
		t.Errorf("404 响应仍泄漏条目元数据: %s", body)
	}
	// A 自己的视图不受 B 的请求影响（A 的列表仍 200 且含 initial 条目）。
	stA, bodyA := signedGet(t, url+"/api/credentials/"+akA+"/sk", akA, skA)
	if stA != http.StatusOK {
		t.Fatalf("A 本人查列表 status = %d, want 200 (body=%s)", stA, bodyA)
	}
	var listA skListResponse
	decodeJSONInto(t, bodyA, &listA)
	if listA.Total < 1 {
		t.Fatalf("A 的列表 total = %d, want >= 1", listA.Total)
	}
}

// ---- sk 删除 ----

// TestCredentials_SKDelete 验证 DELETE /{ak}/sk/{skID}：
//   - 删除后该 SK 签名 401（无其他存活条目的精确场景：只保留被删条目）；
//   - 再删同名 404（幂等失败）；
//   - audit 记录 credential_sk_delete。
func TestCredentials_SKDelete(t *testing.T) {
	var auditBuf bytes.Buffer
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, &auditBuf, nil)

	// renew 一个条目并保留它（initial + renew 共存）；删除 renew 条目后 initial 仍可用。
	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("renew status = %d", status)
	}
	var renewResp renewCredentialResponse
	decodeJSONInto(t, body, &renewResp)

	delURL := url + "/api/credentials/" + testAccessKey + "/sk/" + renewResp.SKID
	st, data := signedDelete(t, delURL, testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("delete sk status = %d, want 200 (body=%s)", st, data)
	}
	// 再删同名 → 404。
	st2, _ := signedDelete(t, delURL, testAccessKey, testAccessSecret)
	if st2 != http.StatusNotFound {
		t.Fatalf("重复 delete sk status = %d, want 404", st2)
	}
	// 旧 initial 条目仍可用。
	st3, _ := signedGet(t, url+"/api/stats", testAccessKey, testAccessSecret)
	if st3 != http.StatusOK {
		t.Fatalf("删除后旧 SK 访问 status = %d, want 200", st3)
	}
	// audit。
	evs := auditActions(t, &auditBuf, auditActionCredSKDelete)
	var foundSuccess, foundError bool
	for _, e := range evs {
		if e["result"] == "success" && e["detail"] == renewResp.SKID {
			foundSuccess = true
		}
		if e["result"] == "error" {
			foundError = true
		}
	}
	if !foundSuccess {
		t.Fatalf("未找到 credential_sk_delete success 审计: %+v", evs)
	}
	if !foundError {
		t.Errorf("未找到 credential_sk_delete error 审计（404 也应留痕）")
	}
}

// ---- sk 过期 ----

// TestCredentials_SKExpire 验证 POST /{ak}/sk/{skID}/expire {until}：
//   - until 过去时间 → 该 SK 立即 401；
//   - 其他条目仍可用。
func TestCredentials_SKExpire(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	// admin 列出 user 的条目找 initial 条目。
	st, data := signedGet(t, url+"/api/credentials/"+testAccessKey+"/sk", testAdminKey, testAdminSecret)
	if st != http.StatusOK {
		t.Fatalf("admin sk list status = %d", st)
	}
	var list skListResponse
	decodeJSONInto(t, data, &list)
	var initialID string
	for _, s := range list.SKs {
		if s.MetaType == "initial" {
			initialID = s.SKID
			break
		}
	}
	if initialID == "" {
		t.Fatalf("未找到 initial 条目: %+v", list.SKs)
	}

	// 先 renew 一个新条目（其 SK 在过期后仍是"其他存活条目"；过期之后再 renew 会因
	// 无存活 SK 而无法认证，故必须在过期前完成）。
	newSKHex := renewNewSKHex(t, url, testAccessKey, testAccessSecret)

	// 过期初始条目（admin 过期 target 条目）。
	expURL := url + "/api/credentials/" + testAccessKey + "/sk/" + initialID + "/expire"
	status, body := doSignedJSON(t, http.MethodPost, expURL, testAdminKey, testAdminSecret, map[string]any{
		"until": time.Now().Add(-time.Hour).Format(time.RFC3339),
	})
	if status != http.StatusOK {
		t.Fatalf("expire status = %d, want 200 (body=%s)", status, body)
	}

	// 该（过期）初始 SK 签名：无 entryID 试签——initial 已过期，用旧 SK 试签所有
	// alive 条目（仅剩 renew 条目）都不匹配 → 401。
	stExpired, _ := signedGet(t, url+"/api/stats", testAccessKey, testAccessSecret)
	if stExpired != http.StatusUnauthorized {
		t.Fatalf("过期 SK 访问 status = %d, want 401", stExpired)
	}

	// "其他条目可用"：renew 的新条目 SK 在过期后仍可用。
	stOK, _ := signedGet(t, url+"/api/stats", testAccessKey, newSKHex)
	if stOK != http.StatusOK {
		t.Fatalf("过期后新条目访问 status = %d, want 200", stOK)
	}
}

// TestCredentials_SKExpire_InvalidUntil 验证 expire 请求体:
//   - until 非法格式 → 400；
//   - until 空串 → 恢复永久有效（零值清除过期）。
func TestCredentials_SKExpire_InvalidUntil(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, "", "", testAccessKey, testAccessSecret, nil, nil)

	st, data := signedGet(t, url+"/api/credentials/"+testAccessKey+"/sk", testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("own sk list status = %d (body=%s)", st, data)
	}
	var list skListResponse
	decodeJSONInto(t, data, &list)
	var initialID string
	for _, s := range list.SKs {
		if s.MetaType == "initial" {
			initialID = s.SKID
			break
		}
	}
	if initialID == "" {
		t.Fatalf("未找到 initial 条目: %+v", list.SKs)
	}
	expURL := url + "/api/credentials/" + testAccessKey + "/sk/" + initialID + "/expire"

	// 1. until 非法格式 → 400。
	status, body := doSignedJSON(t, http.MethodPost, expURL, testAccessKey, testAccessSecret, map[string]any{
		"until": "not-a-time",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("until 非法格式 status = %d, want 400 (body=%s)", status, body)
	}
	if !bytes.Contains(body, []byte("RFC3339")) {
		t.Errorf("400 响应应提示 RFC3339 格式, got %s", body)
	}

	// 2. until 空串 → 恢复永久有效（重复 expire 空 until 应 200，条目仍存活）。
	stOK, b := doSignedJSON(t, http.MethodPost, expURL, testAccessKey, testAccessSecret, map[string]any{
		"until": "",
	})
	if stOK != http.StatusOK {
		t.Fatalf("until 空串恢复 status = %d, want 200 (body=%s)", stOK, b)
	}
	// 条目仍可认证（未被意外过期）。
	stAuth, _ := signedGet(t, url+"/api/stats", testAccessKey, testAccessSecret)
	if stAuth != http.StatusOK {
		t.Fatalf("恢复永久后 SK 访问 status = %d, want 200", stAuth)
	}
}

// renewNewSKHex renew 并返回新 SK 的 hex（解 wrap）。
func renewNewSKHex(t *testing.T, url, ak, oldSKHex string) string {
	t.Helper()
	renewURL := url + "/api/credentials/" + ak + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, ak, oldSKHex, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("renew status = %d (body=%s)", status, body)
	}
	var resp renewCredentialResponse
	decodeJSONInto(t, body, &resp)
	oldSK := mustDecodeHex(t, oldSKHex)
	wkey := testWrapKey(t, oldSK, ak, "")
	newSK, err := accesskey.DecryptSecret(resp.WrappedSecret, wkey)
	if err != nil {
		t.Fatalf("解码新 SK: %v", err)
	}
	return hex.EncodeToString(newSK)
}

// TestCredentials_Renew_AfterSecondRenewOldSKReusable 验证 Important 1 的核心语义：
// renew 的 wrap key 用「调用方签名命中的条目 SK」而非最新 CoreEntry——掉线重发的
// 客户端（仍持上一轮旧 SK，未保存新 SK）再次 renew 时，返回的信封仍能用旧 SK 解开，
// 不因 CoreEntry 已前移（第二轮 renew 把新条目变成 CoreEntry）而断盲盒。
func TestCredentials_Renew_AfterSecondRenewOldSKReusable(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, "", "", testAccessKey, testAccessSecret, nil, nil)
	oldSK := mustDecodeHex(t, testAccessSecret)

	// 第一轮 renew：命中 initial 条目 → 信封用 initial SK 包裹。oldSK 应能解出新 SK1。
	new1 := renewNewSKHex(t, url, testAccessKey, testAccessSecret)
	if new1 == testAccessSecret {
		t.Fatalf("第一轮 renew 应产生新 SK，仍等于旧 SK")
	}

	// 第二轮 renew：此时 CoreEntry 已是最新 alive（SK1 条目）。但调用方仍用旧 SK 签名
	// （无 entryID → 试签命中 initial 条目），wrap 必须用命中条目的 SK（initial）而非
	// CoreEntry——否则新信封用 SK1 包裹，旧 SK 解不开（断盲盒）。
	status, body := doSignedJSON(t, http.MethodPost, url+"/api/credentials/"+testAccessKey+"/renew", testAccessKey, testAccessSecret, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("第二轮 renew status = %d (body=%s)", status, body)
	}
	var resp2 renewCredentialResponse
	decodeJSONInto(t, body, &resp2)
	if resp2.SKID == "" {
		t.Fatalf("第二轮 renew 返回空 sk_id")
	}

	// 关键断言：用调用方仍持有的旧 SK 派生信封密钥解第二轮信封。
	wkey := testWrapKey(t, oldSK, testAccessKey, "")
	new2, err := accesskey.DecryptSecret(resp2.WrappedSecret, wkey)
	if err != nil {
		t.Fatalf("用旧 SK 解第二轮信封失败（wrap 误用最新 CoreEntry，断盲盒）: %v", err)
	}
	if len(new2) != 32 {
		t.Fatalf("第二轮新 SK 长度 = %d, want 32", len(new2))
	}
	// 用解出的新 SK 认证立即可用。
	st, b := signedGet(t, url+"/api/stats", testAccessKey, hex.EncodeToString(new2))
	if st != http.StatusOK {
		t.Fatalf("第二轮新 SK 访问 status = %d, want 200 (body=%s)", st, b)
	}
}

// TestCredentials_PersistFailure 验证持久化失败路径（Important 2）：装配了 store 但
// Save 失败 → renew 返回 500 + 审计 credential_persist_error + 内存态不丢（ring 已
// 更新）。store 与产品 CredentialStore 共享同一 source-of-truth——直接用真实 store
// 指向不可写路径注入必失败，无需引入接口替身。
func TestCredentials_PersistFailure(t *testing.T) {
	var auditBuf bytes.Buffer
	// 用「目录路径上放置同名文件」制造 Save 必失败：MkdirAll(filepath.Dir(path)) 在
	// path 的父目录是普通文件时恒失败（所有平台一致，不依赖只读权限位）。
	base := filepath.Join(t.TempDir(), "store-block")
	if err := os.WriteFile(base, []byte("block"), 0o600); err != nil {
		t.Fatalf("write block file: %v", err)
	}
	store := NewCredentialStore(filepath.Join(base, "credentials.json"))
	url, _, _ := newCredentialsTestServer(t, "", "", testAccessKey, testAccessSecret, &auditBuf, store)

	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	status, body := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{})
	if status != http.StatusInternalServerError {
		t.Fatalf("Save 失败 renew status = %d, want 500 (body=%s)", status, body)
	}
	if bytes.Contains(body, []byte("AppData")) || bytes.Contains(body, []byte("\\\\")) {
		t.Errorf("500 响应不应回传含服务器路径的原始错误: %s", body)
	}
	// 审计 credential_persist_error 留痕。
	if count := len(auditActions(t, &auditBuf, auditActionCredPersistFail)); count < 1 {
		t.Fatalf("未找到 credential_persist_error 审计")
	}
	// 内存态不丢：ring 已追加新条目（AddKey 在 Save 之前成功）——旧 SK 仍可用。
	st, _ := signedGet(t, url+"/api/stats", testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("Save 失败后旧 SK 访问 status = %d, want 200（内存态未丢）", st)
	}
}

// ---- admin 判定边界（步骤 4 补测）----

// TestCredentials_RoleAdmin 验证 getRole 边界：admin 条目存在 → admin；普通用户 → user；
// 未知 AK → user。
func TestCredentials_RoleAdmin(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{CredentialTTL: 30 * 24 * time.Hour})
	h := &Handlers{cfgPtr: cfgPtr, credentialRing: accesskey.NewRing()}
	ask := mustDecodeHex(t, testAdminSecret)
	usk := mustDecodeHex(t, testAccessSecret)
	_ = h.credentialRing.UpsertAK(testAdminKey, "admin")
	_, _ = h.credentialRing.AddKey(testAdminKey, ask, accesskey.WithMeta(accesskey.Meta{Type: "admin"}))
	_ = h.credentialRing.UpsertAK(testAccessKey, "user")
	_, _ = h.credentialRing.AddKey(testAccessKey, usk, accesskey.WithMeta(accesskey.Meta{Type: "initial"}))

	if got := h.getRole(testAdminKey); got != "admin" {
		t.Errorf("getRole(admin) = %q, want admin", got)
	}
	if got := h.getRole(testAccessKey); got != "user" {
		t.Errorf("getRole(user) = %q, want user", got)
	}
	if got := h.getRole("sk-unknown-0000000000000000"); got != "user" {
		t.Errorf("getRole(unknown) = %q, want user", got)
	}
}

// ---- 全量列表 / 新增 / 删除 AK（admin-only）----

// TestCredentials_AKList_Admin 验证 admin 全量列表返回 AK 摘要且不下发明文 SK。
func TestCredentials_AKList_Admin(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)
	st, data := signedGet(t, url+"/api/credentials", testAdminKey, testAdminSecret)
	if st != http.StatusOK {
		t.Fatalf("admin 全量列表 status = %d, want 200 (body=%s)", st, data)
	}
	var resp struct {
		AKs   []akSummary `json:"ak"`
		Total int         `json:"total"`
	}
	decodeJSONInto(t, data, &resp)
	if resp.Total < 2 {
		t.Fatalf("total = %d, want >= 2", resp.Total)
	}
	found := false
	for _, a := range resp.AKs {
		if a.AK == testAccessKey {
			found = true
			if a.SKCount < 1 {
				t.Errorf("user sk_count = %d, want >= 1", a.SKCount)
			}
			if a.AliveSK < 1 {
				t.Errorf("user alive_sk = %d, want >= 1", a.AliveSK)
			}
		}
	}
	if !found {
		t.Fatalf("摘要未包含 user AK: %+v", resp.AKs)
	}
	if bytes.Contains(data, []byte("\"secret\"")) {
		t.Errorf("全量列表响应含 secret 字段（泄露 SK）: %s", data)
	}
}

// TestCredentials_AKAdd_Admin 验证 admin 新增 AK 后新凭据立即可认证；显式 secret 单次
// 返回。
func TestCredentials_AKAdd_Admin(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	newAK := "sk-new-00112233445566aabb"
	newSK := strings.Repeat("11", 32)
	st, body := doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": newAK, "owner": "alice", "secret": newSK,
	})
	if st != http.StatusOK {
		t.Fatalf("admin 新增 AK status = %d, want 200 (body=%s)", st, body)
	}
	var addResp struct {
		AK string `json:"ak"`
	}
	decodeJSONInto(t, body, &addResp)
	if addResp.AK != newAK {
		t.Errorf("新增 AK = %q, want %q", addResp.AK, newAK)
	}
	stAuth, b := signedGet(t, url+"/api/stats", newAK, newSK)
	if stAuth != http.StatusOK {
		t.Fatalf("新增 AK 认证 status = %d, want 200 (body=%s)", stAuth, b)
	}
}

// TestCredentials_AKAdd_Edge 验证新增 AK 边界：
//   - ak 为空 → 400（UpsertAK 校验）；
//   - secret 非 64-hex / 非 32B → 400（hex 解码校验）；
//   - 显式 secret 不回传（响应不含 secret 字段）。
func TestCredentials_AKAdd_Edge(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)

	// 1. ak 为空 → 400。
	st, body := doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": "", "owner": "x",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("空 ak status = %d, want 400 (body=%s)", st, body)
	}

	// 2. secret 非 64-hex → 400。
	st, body = doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": "sk-new-edge-00112233445566", "owner": "x", "secret": "not-hex",
	})
	if st != http.StatusBadRequest {
		t.Fatalf("非法 hex secret status = %d, want 400 (body=%s)", st, body)
	}

	// 3. secret 长度非 32B（64 hex 之外）→ 400。
	st, body = doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": "sk-new-edge-00112233445566", "owner": "x", "secret": strings.Repeat("11", 31),
	})
	if st != http.StatusBadRequest {
		t.Fatalf("secret 非 32B status = %d, want 400 (body=%s)", st, body)
	}

	// 4. 显式 secret 给定 → 200 且响应不回传 secret。
	newAK, newSK := "sk-new-edge-00aabbccddeeff", strings.Repeat("22", 32)
	st, body = doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAdminKey, testAdminSecret, map[string]any{
		"ak": newAK, "owner": "edge", "secret": newSK,
	})
	if st != http.StatusOK {
		t.Fatalf("显式 secret 新增 status = %d, want 200 (body=%s)", st, body)
	}
	if bytes.Contains(body, []byte(newSK)) {
		t.Errorf("响应回传了显式 secret（不应回传）: %s", body)
	}
	// 新凭据立即可认证（secret 真正写入 ring）。
	stAuth, _ := signedGet(t, url+"/api/stats", newAK, newSK)
	if stAuth != http.StatusOK {
		t.Fatalf("新增 AK 认证 status = %d, want 200", stAuth)
	}
}

// TestCredentials_AKDelete_Security 验证 DELETE /api/credentials/{ak} 全链路安全检查。
func TestCredentials_AKDelete_Security(t *testing.T) {
	var auditBuf bytes.Buffer
	url, _, ring := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, &auditBuf, nil)
	target := testAccessKey

	// 1. non-admin 调删除 → 403（admin-only）。
	st, _ := doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+target, testAccessKey, testAccessSecret, map[string]any{
		"confirm": target, "force": true,
	})
	if st != http.StatusForbidden {
		t.Fatalf("user delete AK status = %d, want 403", st)
	}

	// 2. admin：confirm 不匹配 → 400。
	st, body := doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+target, testAdminKey, testAdminSecret, map[string]any{
		"confirm": "sk-wrong", "force": true,
	})
	if st != http.StatusBadRequest {
		t.Fatalf("confirm 不匹配 status = %d, want 400 (body=%s)", st, body)
	}

	// 3. admin：有活跃 SK 且无 force → 400。
	st, body = doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+target, testAdminKey, testAdminSecret, map[string]any{
		"confirm": target,
	})
	if st != http.StatusBadRequest {
		t.Fatalf("有活跃 SK 无 force status = %d, want 400 (body=%s)", st, body)
	}

	// 4. admin：confirm+force → 200 且 AK 删除、认证 401、审计 credential_ak_delete。
	st, body = doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+target, testAdminKey, testAdminSecret, map[string]any{
		"confirm": target, "force": true,
	})
	if st != http.StatusOK {
		t.Fatalf("confirm+force status = %d, want 200 (body=%s)", st, body)
	}
	if _, ok := ring.Lookup(target); ok {
		t.Errorf("删除后 ring 仍含 AK %q", target)
	}
	stAuth, _ := signedGet(t, url+"/api/stats", target, testAccessSecret)
	if stAuth != http.StatusUnauthorized {
		t.Fatalf("删除后原 AK 访问 status = %d, want 401", stAuth)
	}
	if count := len(auditActions(t, &auditBuf, auditActionCredAKDelete)); count < 1 {
		t.Fatalf("未找到 credential_ak_delete 审计")
	}

	// 5. 删除不存在 AK → 404。
	st, _ = doSignedJSON(t, http.MethodDelete, url+"/api/credentials/sk-ghost-0000000000000000", testAdminKey, testAdminSecret, map[string]any{
		"confirm": "sk-ghost-0000000000000000", "force": true,
	})
	if st != http.StatusNotFound {
		t.Fatalf("删除不存在 AK status = %d, want 404", st)
	}
}

// ---- 4A 无 admin：admin-only 恒 403 ----

// TestCredentials_NoAdmin_Hard403 验证 4A 无 admin 部署（ring 只有普通用户条目）时
// admin-only 端点恒 403（GET /api/credentials、POST /api/credentials、DELETE /{ak}）。
func TestCredentials_NoAdmin_Hard403(t *testing.T) {
	url, _ := newTestServerWithAllRoutesCreds(t, nil)

	st, body := signedGet(t, url+"/api/credentials", testAccessKey, testAccessSecret)
	if st != http.StatusForbidden {
		t.Fatalf("无 admin 全量列表 status = %d, want 403 (body=%s)", st, body)
	}
	st, body = doSignedJSON(t, http.MethodPost, url+"/api/credentials", testAccessKey, testAccessSecret, map[string]any{
		"ak": "sk-new-0000000000000000", "owner": "x",
	})
	if st != http.StatusForbidden {
		t.Fatalf("无 admin POST status = %d, want 403 (body=%s)", st, body)
	}
	st, body = doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+testAccessKey, testAccessKey, testAccessSecret, map[string]any{
		"confirm": testAccessKey, "force": true,
	})
	if st != http.StatusForbidden {
		t.Fatalf("无 admin DELETE AK status = %d, want 403 (body=%s)", st, body)
	}
}

// TestCredentials_Renew_StillOwnOnlyInNoAdmin 验证即使无 admin，renew/own sk 列表
// 作为本人操作仍可用（权限分档不因无 admin 而全锁）。
func TestCredentials_Renew_StillOwnOnlyInNoAdmin(t *testing.T) {
	url, _ := newTestServerWithAllRoutesCreds(t, nil)
	renewURL := url + "/api/credentials/" + testAccessKey + "/renew"
	st, _ := doSignedJSON(t, http.MethodPost, renewURL, testAccessKey, testAccessSecret, map[string]any{})
	if st != http.StatusOK {
		t.Fatalf("无 admin 部署本人 renew status = %d, want 200", st)
	}
	st2, _ := signedGet(t, url+"/api/credentials/"+testAccessKey+"/sk", testAccessKey, testAccessSecret)
	if st2 != http.StatusOK {
		t.Fatalf("无 admin 部署本人 sk 列表 status = %d, want 200", st2)
	}
}

// ---- 认证保护 / 隧道可达 ----

// TestCredentials_NoAuthUnauthorized 验证主 mux（direct 面）无凭据访问凭据管理端点 401。
func TestCredentials_NoAuthUnauthorized(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)
	req, _ := http.NewRequest(http.MethodGet, url+"/api/credentials", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no-auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据 GET /api/credentials 应 401, got %d", resp.StatusCode)
	}
}

// TestCredentials_NonOwnerRenewNotFound 验证 renew 仅限本人：admin renew user 的 AK → 404。
func TestCredentials_NonOwnerRenewNotFound(t *testing.T) {
	url, _, _ := newCredentialsTestServer(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret, nil, nil)
	st, _ := doSignedJSON(t, http.MethodPost, url+"/api/credentials/"+testAccessKey+"/renew", testAdminKey, testAdminSecret, map[string]any{})
	if st != http.StatusNotFound {
		t.Fatalf("admin renew user 的 AK status = %d, want 404", st)
	}
}

// TestCredentials_Renew_RingNil_500 验证凭据 Ring 未装配（服务端配置错误）时 renew
// 返回 500 而非泛化的 400（Minor 6：errCredentialRingUnavailable → InternalServerError）。
func TestCredentials_Renew_RingNil_500(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:         http.NewServeMux(),
		CfgPtr:      &cfgPtr,
		Version:     "v",
		BuildAt:     "b",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
		// 不注入 CredentialRing → authMiddleware 无凭据时按未认证处理（401，见 auth.go
		// handleNoCredentials）；本测试经 localMux（隧道内层）直连 handler：localMux
		// 裸注册不经 authMiddleware，renew 命中 h.credentialRing==nil →
		// errCredentialRingUnavailable → 500（Minor 6 目标路径）。
	})
	ts := httptest.NewServer(h.LocalHandler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	// localMux 侧（LocalHandler）无 authMiddleware → 不验 SproxySig、actor 为空；
	// handler 的本人判定在 actor=="" 时返回 404（不泄露），因此 ring-nil 的 500 映射
	// 无法经路由触达。直接构造 Handlers 白盒验证 renewCredentialHandler 的
	// errCredentialRingUnavailable → 500 映射（Minor 6 目标路径）。
	// 注意：actor 必须 == target（PathValue"ak"），否则 actor 前置检查先 404。
	h2 := &Handlers{cfgPtr: &cfgPtr, credentialRing: nil}
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/"+testAccessKey+"/renew", nil)
	req.SetPathValue("ak", testAccessKey)
	req = req.WithContext(withActor(req.Context(), testAccessKey))
	rr := httptest.NewRecorder()
	h2.renewCredentialHandler(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("ring 未装配 renew status = %d, want 500 (body=%s)", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(errCredentialRingUnavailable.Error())) {
		t.Errorf("500 响应应含 %q, got %s", errCredentialRingUnavailable.Error(), rr.Body.String())
	}
}
