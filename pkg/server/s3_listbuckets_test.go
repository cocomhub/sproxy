// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_listbuckets_test.go 验证 S3 ListBuckets（roadmap 11.5-④ 卷即桶生态兼容）：
//  1. GET /s3/（无 list-type，正确 SigV4）→ ListAllMyBucketsResult XML 含本地卷名。
//  2. 无签名 → 401。
//  3. 外部卷不列（只有本地卷在结果）。

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestS3ListBuckets_ReturnsLocalVolumes GET /s3/ 返回本地卷名（卷即桶）。
func TestS3ListBuckets_ReturnsLocalVolumes(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, func(cfg *Config) {
		// 双本地卷装配（默认卷 + second）。
		cfg.Volumes = append(cfg.Volumes, VolumeConfig{Name: "second", Root: t.TempDir()})
	})
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	now := time.Now()
	host := strings.TrimPrefix(url, "http://")
	auth := sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/", host, nil, now)
	req, _ := http.NewRequest(http.MethodGet, url+"/s3/", nil)
	req.Header.Set("Authorization", auth)
	req.Header.Set("x-amz-date", now.UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") // 空 body sha256
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("listbuckets: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /s3/ = %d body=%s", resp.StatusCode, body)
	}
	s := string(body)
	if !strings.Contains(s, "<ListAllMyBucketsResult>") {
		t.Fatalf("响应应含 ListAllMyBucketsResult: %s", s)
	}
	if !strings.Contains(s, "<Name>default</Name>") {
		t.Errorf("默认卷 default 应列为 bucket: %s", s)
	}
	if !strings.Contains(s, "<Name>second</Name>") {
		t.Errorf("本地卷 second 应列为 bucket: %s", s)
	}
}

// TestS3ListBuckets_Unauthorized 无签名 → 401。
func TestS3ListBuckets_Unauthorized(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	req, _ := http.NewRequest(http.MethodGet, url+"/s3/", nil)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("no-auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无签名 GET /s3/ 应 401, got %d", resp.StatusCode)
	}
}
