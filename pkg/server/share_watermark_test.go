// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// share_watermark_test.go 验证分享绑定水印种子（roadmap P2 残余）：
//  1. 创建分享带 watermark → 持久化 + 访问响应带 X-Share-Watermark 头。
//  2. 无 watermark → 无头（零回归）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestShareWatermark_Header 创建带水印分享 → 访问响应含 X-Share-Watermark。
func TestShareWatermark_Header(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	if st := uploadFileSigned(t, baseURL, "wm.png", []byte("img-bytes")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// 创建分享（带 watermark）。
	body := `{"filename":"wm.png","watermark":"trace-123"}`
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/share", bytes.NewReader([]byte(body)))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("share = %d", resp.StatusCode)
	}
	var sr struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	if jerr := json.NewDecoder(resp.Body).Decode(&sr); jerr != nil {
		t.Fatal(jerr)
	}
	if !sr.Success || sr.Token == "" {
		t.Fatalf("share 响应 = %+v", sr)
	}
	// 访问分享。
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/s/"+sr.Token, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	io.Copy(io.Discard, resp2.Body)
	if resp2.Header.Get("X-Share-Watermark") != "trace-123" {
		t.Fatalf("X-Share-Watermark = %q, want trace-123", resp2.Header.Get("X-Share-Watermark"))
	}
}

// TestShareWatermark_NoSeed 无 watermark → 无头（零回归）。
func TestShareWatermark_NoSeed(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	if st := uploadFileSigned(t, baseURL, "plain.png", []byte("img")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	body := `{"filename":"plain.png"}`
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/share", bytes.NewReader([]byte(body)))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Token == "" {
		t.Fatalf("share 无 token")
	}
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/s/"+sr.Token, nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	io.Copy(io.Discard, resp2.Body)
	if resp2.Header.Get("X-Share-Watermark") != "" {
		t.Fatalf("无 watermark 不应带头, got %q", resp2.Header.Get("X-Share-Watermark"))
	}
}
