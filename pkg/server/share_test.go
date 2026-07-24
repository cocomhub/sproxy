// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
	json.NewDecoder(resp.Body).Decode(&shareResp)
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
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for expired link, got %d", resp2.StatusCode)
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
	ss := NewShareStore()
	// 填满上限，所有条目立即过期（TTL=0 => ExpiresAt ≈ now，eviction 时已过期）
	for i := range maxShareEntries {
		_, err := ss.Create(fmt.Sprintf("file%d.txt", i), "/tmp/file", 0, 0, false)
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// 触发 eviction：应该成功删除过期条目再新增
	link, err := ss.Create("newfile.txt", "/tmp/file", time.Hour, 0, false)
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
	ss := NewShareStore()
	// 填满上限，所有条目 1 小时后才过期（eviction 时无过期条目）
	for i := range maxShareEntries {
		_, err := ss.Create(fmt.Sprintf("file%d.txt", i), "/tmp/file", time.Hour, 0, false)
		if err != nil {
			t.Fatalf("unexpected error at iteration %d: %v", i, err)
		}
	}
	// 无过期条目可淘汰，应该返回错误
	_, err := ss.Create("overflow.txt", "/tmp/file", time.Hour, 0, false)
	if err == nil {
		t.Error("expected error when share store is full with no expired entries")
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
	json.NewDecoder(resp.Body).Decode(&shareResp)
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
	json.NewDecoder(resp.Body).Decode(&shareResp)
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
