// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_list_test.go 验证 S3 ListObjectsV2 + HeadObject（roadmap P2 S3 兼容服务端扩展）：
//  1. GET /s3/?list-type=2 → XML ListBucketResult（含已上传对象）。
//  2. HEAD /s3/<key> → 200 + Content-Length（HeadObject 元信息）。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3Server_ListObjectsV2 列表 + HeadObject。
func TestS3Server_ListObjectsV2(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")

	// 上传两个文件。
	for _, name := range []string{"a.txt", "b.txt"} {
		body := []byte("content " + name)
		auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/"+name, host, body, now)
		req, _ := http.NewRequest(http.MethodPut, url+"/s3/"+name, bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
		req.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(body)))
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 201 && resp.StatusCode != 200 {
			t.Fatalf("put %s = %d", name, resp.StatusCode)
		}
	}

	// ListObjectsV2（GET /s3/?list-type=2）。
	authL := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/?list-type=2", host, nil, now)
	reqL, _ := http.NewRequest(http.MethodGet, url+"/s3/?list-type=2", nil)
	reqL.Header.Set("Authorization", authL)
	reqL.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqL.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	respL, err := cl.Do(reqL)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	bodyL, _ := io.ReadAll(respL.Body)
	respL.Body.Close()
	if respL.StatusCode != 200 {
		t.Fatalf("list = %d body=%s", respL.StatusCode, bodyL[:min(len(bodyL), 200)])
	}
	if !strings.Contains(string(bodyL), "a.txt") || !strings.Contains(string(bodyL), "b.txt") {
		t.Fatalf("ListObjectsV2 应含 a.txt/b.txt: %s", bodyL[:min(len(bodyL), 300)])
	}
	if !strings.Contains(string(bodyL), "ListBucketResult") {
		t.Fatalf("应为 ListBucketResult XML: %s", bodyL[:min(len(bodyL), 100)])
	}

	// HeadObject（HEAD /s3/a.txt）。
	authH := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodHead, "/s3/a.txt", host, nil, now)
	reqH, _ := http.NewRequest(http.MethodHead, url+"/s3/a.txt", nil)
	reqH.Header.Set("Authorization", authH)
	reqH.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	reqH.Header.Set("x-amz-content-sha256", fmt.Sprintf("%x", sha256sum(nil)))
	respH, err := cl.Do(reqH)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	io.Copy(io.Discard, respH.Body)
	respH.Body.Close()
	if respH.StatusCode != 200 {
		t.Fatalf("head = %d", respH.StatusCode)
	}
	if respH.Header.Get("Content-Length") == "" {
		t.Fatalf("head 应含 Content-Length")
	}
}
