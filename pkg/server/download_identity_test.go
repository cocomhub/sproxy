// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// download_identity_test.go 验证服务端为「客户端断点续传的同一文件校验」提供的能力
// （2026-11-07 用户要求：必须确保是同一个文件才能续传，文件变化则从零下载）：
//
//  1. GET /download 返回强校验器 ETag（sha256 的 quoted 形式）+ X-File-Checksum；
//  2. `Range` + 匹配的 `If-Range` ⇒ 206（正常续传）；
//  3. `Range` + 不匹配的 `If-Range` ⇒ **200 全量**（文件已换，绝不能回错位区间）；
//  4. 无 `If-Range` 的 `Range` 仍 206（零回归）。
//
// httptest 使用随机端口（127.0.0.1:0），端到端走真实 HTTP 栈。

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestDownload_ReturnsETagAndHonorsIfRange 是服务端侧的能力契约（客户端依赖它实现
// 「换文件即从零重下」）。
func TestDownload_ReturnsETagAndHonorsIfRange(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	content := bytes.Repeat([]byte("sproxy-if-range-"), 512) // 8 KiB
	sum := sha256hex(content)
	status, respBody := uploadFile(t, url, "ifrange.bin", content, map[string]string{
		headerFileChecksum: sum,
	})
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	get := func(t *testing.T, rangeHdr, ifRange string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, url+"/download?filename=ifrange.bin", nil)
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.Header.Set(headerFileChecksum, sum)
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		if ifRange != "" {
			req.Header.Set("If-Range", ifRange)
		}
		resp, derr := testHTTPClient(t).Do(req)
		if derr != nil {
			t.Fatalf("download: %v", derr)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	wantETag := `"` + sum + `"`

	t.Run("全量下载返回 ETag 与 checksum", func(t *testing.T) {
		t.Parallel()
		resp, body := get(t, "", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("应 200, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("ETag"); got != wantETag {
			t.Fatalf("ETag 应为 %q（客户端续传校验器），got %q", wantETag, got)
		}
		if got := resp.Header.Get(headerFileChecksum); got != sum {
			t.Fatalf("X-File-Checksum 应为 %q, got %q", sum, got)
		}
		if !bytes.Equal(body, content) {
			t.Fatalf("全量内容不符：%d bytes", len(body))
		}
	})

	t.Run("Range+匹配 If-Range ⇒ 206 续传片段", func(t *testing.T) {
		t.Parallel()
		resp, body := get(t, "bytes=100-", wantETag)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("校验器匹配应 206, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Range"); got != "bytes 100-"+
			strconv.Itoa(len(content)-1)+"/"+strconv.Itoa(len(content)) {
			t.Fatalf("Content-Range 异常: %q", got)
		}
		if !bytes.Equal(body, content[100:]) {
			t.Fatalf("续传片段不符：got %d bytes want %d", len(body), len(content)-100)
		}
	})

	t.Run("Range+不匹配 If-Range ⇒ 200 全量（换文件从零重下）", func(t *testing.T) {
		t.Parallel()
		resp, body := get(t, "bytes=100-", `"deadbeef"`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("校验器不匹配必须回 200 全量（否则客户端会拼接错位区间），got %d", resp.StatusCode)
		}
		if !bytes.Equal(body, content) {
			t.Fatalf("全量内容不符：got %d bytes want %d", len(body), len(content))
		}
	})

	t.Run("无 If-Range 的 Range 仍 206（零回归）", func(t *testing.T) {
		t.Parallel()
		resp, body := get(t, "bytes=100-", "")
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("无 If-Range 应保持 206 语义, got %d", resp.StatusCode)
		}
		if !bytes.Equal(body, content[100:]) {
			t.Fatalf("续传片段不符：got %d bytes", len(body))
		}
	})
}

// TestChunkDownload_CarriesFileChecksumAndETag 验证分块响应同样携带整文件校验器
// （客户端据此在分块传输中途发现「文件已换」并中止，而不是把两个文件拼起来）。
func TestChunkDownload_CarriesFileChecksumAndETag(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	content := bytes.Repeat([]byte("sproxy-chunk-identity-"), 256)
	sum := sha256hex(content)
	if status, body := uploadFile(t, url, "chunkid.bin", content, map[string]string{
		headerFileChecksum: sum,
	}); status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, body)
	}

	req, err := http.NewRequest(http.MethodGet,
		url+"/download/chunk?filename=chunkid.bin&offset=0&length=1024", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set(headerFileChecksum, sum)
	resp, derr := testHTTPClient(t).Do(req)
	if derr != nil {
		t.Fatalf("chunk: %v", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("分块应 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get(headerFileChecksum); got != sum {
		t.Fatalf("分块响应 X-File-Checksum 应为整文件 sha256 %q, got %q", sum, got)
	}
	if got := resp.Header.Get("ETag"); got != `"`+sum+`"` {
		t.Fatalf("分块响应 ETag 应为 %q, got %q", `"`+sum+`"`, got)
	}
	if got := resp.Header.Get("X-Chunk-Checksum"); strings.TrimSpace(got) == "" {
		t.Fatal("分块响应缺少 X-Chunk-Checksum（传输完整性校验）")
	}
}
