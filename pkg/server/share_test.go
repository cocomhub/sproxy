// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
	if err := json.NewDecoder(resp.Body).Decode(&shareResp); err != nil {
		t.Fatal(err)
	}
	token, ok := shareResp["token"].(string)
	if !ok || token == "" {
		t.Fatal("expected non-empty token")
	}

	// 访问分享链接（不跟随重定向，验证 302）
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp2, err := client.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", resp2.StatusCode)
	}
	loc := resp2.Header.Get("Location")
	if !strings.Contains(loc, "shared.txt") {
		t.Fatalf("expected redirect to shared.txt, got %s", loc)
	}
}

func TestShare_Expired(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

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

	// 访问应返回 410 Gone
	resp2, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 for expired link, got %d", resp2.StatusCode)
	}
}

func TestShare_MissingFilename(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
