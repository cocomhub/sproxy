// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// gzip_accept_test.go 验证按内容类型自动 gzip（roadmap P2 残余）：
//  1. gzipEligible 白名单（text/* → true；image/* → false）。
//  2. 文件面 download 响应 Accept-Encoding: gzip → Content-Encoding: gzip。

import (
	"compress/gzip"
	"io"
	"net/http"
	"testing"
)

// TestGzipEligible 白名单判定。
func TestGzipEligible(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ct  string
		exp bool
	}{
		{"text/plain", true},
		{"application/json", true},
		{"image/png", false},
		{"video/mp4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := gzipEligible(c.ct); got != c.exp {
			t.Fatalf("gzipEligible(%q) = %v, want %v", c.ct, got, c.exp)
		}
	}
}

// TestDownloadGzip 文件面下载自动 gzip（文本文件）。
func TestDownloadGzip(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	// 上传文本文件。
	if st := uploadFileSigned(t, baseURL, "doc.txt", []byte("hello world hello world")); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename=doc.txt", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("应 Content-Encoding: gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
	zr, zerr := gzip.NewReader(resp.Body)
	if zerr != nil {
		t.Fatalf("gzip reader: %v", zerr)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != "hello world hello world" {
		t.Fatalf("gzip 解压 body = %q", body)
	}
}
