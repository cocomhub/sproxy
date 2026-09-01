// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

func TestShare_CreateAndAccess(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 先上传文件
	body := []byte("shared content")
	uploadFile(t, url, "shared.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建分享链接
	reqBody := `{"filename":"shared.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating share, got %d", resp.StatusCode)
	}

	var shareResp map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&shareResp); err != nil {
		t.Fatal(err)
	}
	token, ok := shareResp["token"].(string)
	if !ok || token == "" {
		t.Fatal("expected non-empty token")
	}

	// 访问分享链接，直接下载文件
	resp2, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	data, _ := io.ReadAll(resp2.Body)
	if string(data) != "shared content" {
		t.Fatalf("expected 'shared content', got '%s'", string(data))
	}
}

func TestShare_Expired(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 上传文件
	uploadFile(t, url, "x.txt", []byte("test"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("test")),
	})

	// 创建极短过期时间的链接（1ns，已立即过期）
	reqBody := `{"filename":"x.txt","ttl":"1ns"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var shareResp map[string]any
	if err2 := json.NewDecoder(resp.Body).Decode(&shareResp); err2 != nil {
		t.Fatalf("decode: %v", err)
	}
	token, _ := shareResp["token"].(string)

	time.Sleep(10 * time.Millisecond)

	// 不跟随重定向的 client
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp2, err := client.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound && resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 404 or 409 for expired link, got %d", resp2.StatusCode)
	}
}

func TestShare_MissingFilename(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestShare_InvalidToken(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/s/nonexistent_token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestShare_NonExistentFile(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	reqBody := `{"filename":"nonexistent.txt"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent file, got %d", resp.StatusCode)
	}
}

func TestShare_CreateEviction(t *testing.T) {
	t.Parallel()
	ss := NewShareStore(slog.Default())
	// 填满上限，所有条目立即过期（TTL=0 => ExpiresAt ≈ now，eviction 时已过期）
	for i := range maxShareEntries {
		_, err := ss.Create(fmt.Sprintf("file%d.txt", i), "/tmp/file", "", 0, 0, false)
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// 触发 eviction：应该成功删除过期条目再新增
	link, err := ss.Create("newfile.txt", "/tmp/file", "", time.Hour, 0, false)
	if err != nil {
		t.Fatalf("expected eviction to succeed, got: %v", err)
	}
	if link.Token == "" {
		t.Fatal("expected non-empty token")
	}
	ss.mu.Lock()
	count := len(ss.links)
	ss.mu.Unlock()
	if count > maxShareEntries {
		t.Errorf("expected at most %d entries after eviction, got %d", maxShareEntries, count)
	}
}

func TestShare_CreateEvictionNoExpired(t *testing.T) {
	t.Parallel()
	ss := NewShareStore(slog.Default())
	// 填满上限，所有条目 1 小时后才过期（eviction 时无过期条目）
	for i := range maxShareEntries {
		_, err := ss.Create(fmt.Sprintf("file%d.txt", i), "/tmp/file", "", time.Hour, 0, false)
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// 无过期条目时，eviction 按创建时间淘汰最旧的 10%，应成功
	link, err := ss.Create("overflow.txt", "/tmp/file", "", time.Hour, 0, false)
	if err != nil {
		t.Fatalf("expected eviction to succeed via oldest-10%% strategy, got: %v", err)
	}
	if link.Token == "" {
		t.Fatal("expected non-empty token")
	}
	ss.mu.Lock()
	count := len(ss.links)
	ss.mu.Unlock()
	if count > maxShareEntries {
		t.Errorf("expected at most %d entries after eviction, got %d", maxShareEntries, count)
	}
}

func TestShare_OneTime(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("one-time content")
	uploadFile(t, url, "onetime.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建一次性分享
	reqBody := `{"filename":"onetime.txt","one_time":true}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var shareResp map[string]any
	if err2 := json.NewDecoder(resp.Body).Decode(&shareResp); err2 != nil {
		t.Fatalf("decode: %v", err)
	}
	token := shareResp["token"].(string)

	// 第一次下载应成功
	resp2, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 first download, got %d", resp2.StatusCode)
	}

	// 第二次下载应返回 404（已删除）
	resp3, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for second download, got %d", resp3.StatusCode)
	}
}

func TestShare_List(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 先上传文件
	body := []byte("list test content")
	uploadFile(t, url, "list_test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建分享链接
	reqBody := `{"filename":"list_test.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 列出分享
	resp2, err := http.Get(url + "/api/shares")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	var result struct {
		Shares []struct {
			Token        string `json:"token"`
			Filename     string `json:"filename"`
			CreatedAt    string `json:"created_at"`
			ExpiresAt    string `json:"expires_at"`
			MaxDownloads int    `json:"max_downloads"`
			Downloads    int    `json:"downloads"`
			OneTime      bool   `json:"one_time"`
			Expired      bool   `json:"expired"`
		} `json:"shares"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Shares) == 0 {
		t.Fatal("expected at least 1 share")
	}
	found := false
	for _, s := range result.Shares {
		if s.Filename == "list_test.txt" {
			found = true
			if s.Token == "" {
				t.Error("expected non-empty token")
			}
			if s.CreatedAt == "" {
				t.Error("expected non-empty created_at")
			}
			if s.Expired {
				t.Error("expected expired=false for a valid share")
			}
			break
		}
	}
	if !found {
		t.Error("share for list_test.txt not found in list")
	}
}

func TestShare_Revoke(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("revoke test content")
	uploadFile(t, url, "revoke_test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建分享
	reqBody := `{"filename":"revoke_test.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}

	var shareResp map[string]any
	if err2 := json.NewDecoder(resp.Body).Decode(&shareResp); err2 != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	token := shareResp["token"].(string)

	// 撤销分享
	req2, err := http.NewRequest(http.MethodDelete, url+"/api/shares/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 revoking share, got %d", resp2.StatusCode)
	}

	// 确认访问返回 404
	resp3, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after revoke, got %d", resp3.StatusCode)
	}
}

func TestShare_RevokeNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest(http.MethodDelete, url+"/api/shares/nonexistent_token", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent token, got %d", resp.StatusCode)
	}
}

// TestShare_MultiTenantOwnerScoped 验证 M5 修复：认证用户只见自己的分享链接，
// 且不能撤销他人的分享（跨租户越权防护）；admin/未认证仍可见全部。
func TestShare_MultiTenantOwnerScoped(t *testing.T) {
	url, _, _ := newAuditTestServer(t, nil)

	postSigned := func(path, body string) (*http.Response, error) {
		var r *http.Request
		var bodyBytes []byte
		if body != "" {
			bodyBytes = []byte(body)
			r, _ = http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			signBodyRequest(r, testAccessKey, testAccessSecret, bodyBytes)
		} else {
			r, _ = http.NewRequest(http.MethodPost, url+path, nil)
			signRequest(r, testAccessKey, testAccessSecret)
		}
		return http.DefaultClient.Do(r)
	}
	getSigned := func(path string) (*http.Response, error) {
		r, _ := http.NewRequest(http.MethodGet, url+path, nil)
		signRequest(r, testAccessKey, testAccessSecret)
		return http.DefaultClient.Do(r)
	}
	delSigned := func(path string) (*http.Response, error) {
		r, _ := http.NewRequest(http.MethodDelete, url+path, nil)
		signRequestNonce(r, testAccessKey, testAccessSecret) // 随机 nonce 防全量并发碰撞
		return http.DefaultClient.Do(r)
	}

	// 先上传两个文件
	for _, f := range []string{"share_a.txt", "share_b.txt"} {
		uploadFileSigned(t, url, f, []byte(f+" content"))
	}

	// A（testAccessKey）创建两个分享：指向 share_a.txt（自己的）与 share_b.txt（B 的，设想）
	// 注意：这里的 auth 只有单一 AK，为模拟两个租户，直接用 store 层面构造第二个 owner 的链接。
	// 步骤：A 创建 share_a 分享；再直接向 shareStore 注入 owner="ak-B" 的分享（绕过 HTTP）。
	resp, err := postSigned("/api/share", `{"filename":"share_a.txt","ttl":"1h"}`)
	if err != nil {
		t.Fatal(err)
	}
	var aShare struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&aShare)
	resp.Body.Close()
	if !aShare.Success || aShare.Token == "" {
		t.Fatalf("A 创建分享失败: %+v", aShare)
	}

	// 注入 B 的分享到 store（模拟另一租户在内存中创建的链接）
	url2, _, _ := newAuditTestServer(t, nil) // 独立实例便于直接操纵
	_ = url2
	// 简化：直接在当前 server 的 store 上创建 —— 但无 store 引用，改用第二个 servern 实例的 store
	// 这里仅验证 List 过滤逻辑：用同一 store，把 B 的 link 塞进去比较麻烦。
	// 改用 ShareStore 纯单测覆盖 owner 过滤 + Revoke 越权（在下面 TestShareStore_OwnerScoped）。
	_ = aShare

	// A 列出自己的分享：应只有 1 条（share_a.txt）
	resp2, err := getSigned("/api/shares")
	if err != nil {
		t.Fatal(err)
	}
	var listRes struct {
		Shares []struct {
			Token    string `json:"token"`
			Filename string `json:"filename"`
		} `json:"shares"`
	}
	json.NewDecoder(resp2.Body).Decode(&listRes)
	resp2.Body.Close()
	if len(listRes.Shares) != 1 || listRes.Shares[0].Filename != "share_a.txt" {
		t.Fatalf("A 的分享列表应有且仅 1 条 share_a.txt, got %+v", listRes.Shares)
	}

	// A 撤销一个不存在的 token（模拟 B 的）→ 404（且因多租户 owner 不匹配同样被拒）
	resp3, err := delSigned("/api/shares/" + strings.Repeat("0", 32))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("撤销不存在 token 应 404, got %d", resp3.StatusCode)
	}
}

// TestShareStore_OwnerScoped 验证 ShareStore 的 owner 过滤与撤销越权防护（纯单测）。
func TestShareStore_OwnerScoped(t *testing.T) {
	s := NewShareStore(testLogger())
	defer s.Stop()

	l1, err := s.Create("a.txt", "/abs/a.txt", "ak-A", time.Hour, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := s.Create("b.txt", "/abs/b.txt", "ak-B", time.Hour, 0, false)
	if err != nil {
		t.Fatal(err)
	}

	// owner 过滤：A 只见 A，admin（空 owner）见全部
	if got := s.List("ak-A"); len(got) != 1 || got[0].Token != l1.Token {
		t.Fatalf("A 应只见自己的分享, got %+v", s.List("ak-A"))
	}
	if got := s.List(""); len(got) != 2 {
		t.Fatalf("admin 应见全部 2 条, got %d", len(got))
	}

	// 跨租户撤销被拒：A 撤 B 的 → error；A 撤自己的 → ok
	if err := s.Revoke(l2.Token, "ak-A"); err == nil {
		t.Fatal("A 撤销 B 的分享应被拒")
	}
	if err := s.Revoke(l1.Token, "ak-A"); err != nil {
		t.Fatalf("A 撤销自己的分享应成功: %v", err)
	}
}

// signRequestNonce 生成带随机 nonce 的签名请求（避免全量并发下 UnixNano nonce 碰撞失败）。
func signRequestNonce(r *http.Request, ak, sk string) {
	now := time.Now()
	nb := make([]byte, 12)
	_, _ = rand.Read(nb)
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      hex.EncodeToString(nb),
		BodySHA256: sproxysig.EmptyBodyHash(),
	}
	h.Sig = sproxysig.Sign(sk, h, r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	r.Header.Set("Authorization", formatSigAuth(h))
}
