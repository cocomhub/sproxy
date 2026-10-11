// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// gzip_accept_test.go 验证按内容类型自动 gzip（roadmap P2 残余）：
//  1. gzipEligible 白名单（text/* → true；image/* → false）。
//  2. 文件面 download 响应**跳过 gzip**（P1 对抗评审：gzip 会把读校验失败后的「已校验
//     前缀」打成无 Content-Length 的合法 gzip 流 → 静默截断；跳过后保留 Content-Length，
//     客户端可判截断且 fail-closed 生效）。

import (
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
		{contentTypeJSON, true},
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

// TestDownloadGzipSkipped 文件面下载必须跳过 gzip（fail-closed：校验失败时客户端能
// 通过 Content-Length/非 gzip 流感知截断，而不是拿到格式合法的截断 gzip）。
func TestDownloadGzipSkipped(t *testing.T) {
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
	if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
		t.Fatalf("文件面下载不应 gzip（fail-closed 需可判截断），got Content-Encoding=%q", ce)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world hello world" {
		t.Fatalf("body = %q", body)
	}
	if cl := resp.Header.Get("Content-Length"); cl == "" {
		t.Fatal("跳过 gzip 后应保留 Content-Length（可判截断）")
	}
}
