// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
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
