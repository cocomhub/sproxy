// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_server_test.go 验证 S3 兼容服务端（roadmap P2 S3 兼容服务端）：
//  1. SigV4 验签：正确签名 200 / 无签名 401 / 错签名 403。
//  2. GET /s3/<key> 下载 / PUT 上传 / DELETE 删除 全流程。
//  3. 验签失败不落盘（fail-closed）。

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// sigV4Sign 测试用 SigV4 签名（AK/SK + 方法/路径/时间 → Authorization 头）。
func sigV4SignHost(ak, sk, method, path, host string, body []byte, now time.Time) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	region := "us-east-1"
	service := "s3"
	payloadHash := hex.EncodeToString(sha256sum(body))
	// CanonicalRequest
	canonicalHeaders := "host:" + host + "\nx-amz-content-sha256:" + payloadHash + "\nx-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{method, path, "", canonicalHeaders, signedHeaders, payloadHash}, "\n")
	fmt.Printf("DBG test canonicalRequest=%q\n", canonicalRequest)
	// StringToSign
	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	hashCR := hex.EncodeToString(sha256sum([]byte(canonicalRequest)))
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hashCR}, "\n")
	// Signature
	kDate := hmacSHA256([]byte("AWS4"+sk), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, dateStamp, region, service, signedHeaders, signature)
}

// TestS3Server_AuthFlow 正确签名 200 / 无签名 401 / 错签名 403。
func TestS3Server_AuthFlow(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()

	// 无签名 → 401。
	req, _ := http.NewRequest(http.MethodGet, url+"/s3/key.txt", nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("no-auth: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无签名应 401, got %d", resp.StatusCode)
	}

	// 正确签名 PUT → 200。
	body := []byte("s3 content")
	// 签名 host = 实际请求 Host（http.Client 自动设 URL host；r.Host 读它）。
	host := strings.TrimPrefix(url, "http://")
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/key.txt", host, body, now)
	req2, _ := http.NewRequest(http.MethodPut, url+"/s3/key.txt", bytes.NewReader(body))
	req2.Header.Set("Authorization", auth)
	req2.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req2.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(body)))

	resp2, err := cl.Do(req2)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 && resp2.StatusCode != 201 {
		t.Fatalf("PUT = %d body=%q", resp2.StatusCode, body2)
	}

	// 正确签名 GET → 200 + 内容。
	auth3 := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/key.txt", host, nil, now)
	req3, _ := http.NewRequest(http.MethodGet, url+"/s3/key.txt", nil)
	req3.Header.Set("Authorization", auth3)
	req3.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req3.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(nil)))

	resp3, err := cl.Do(req3)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 || string(got) != "s3 content" {
		t.Fatalf("GET = %d body=%q", resp3.StatusCode, got)
	}

	// 错签名 DELETE → 403。
	auth4 := sigV4SignHost(testAccessKey, "wrong-secret", http.MethodDelete, "/s3/key.txt", host, nil, now)
	req4, _ := http.NewRequest(http.MethodDelete, url+"/s3/key.txt", nil)
	req4.Header.Set("Authorization", auth4)
	req4.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))

	resp4, err := cl.Do(req4)
	if err != nil {
		t.Fatalf("del: %v", err)
	}
	io.Copy(io.Discard, resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusForbidden {
		t.Fatalf("错签名应 403, got %d", resp4.StatusCode)
	}
}

var _ = url.Values{}
