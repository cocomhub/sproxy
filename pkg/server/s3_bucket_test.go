// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_bucket_test.go 验证 S3 多桶语义（roadmap P2 残余）：
//  1. /s3/<卷名>/<key> → 映射该卷 user 桶（PUT/GET）。
//  2. 单段 key（无卷名）→ 默认卷（零回归）。

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestS3Bucket_RouteToVolume 多桶：/s3/<vol>/<key> 落该卷。
func TestS3Bucket_RouteToVolume(t *testing.T) {
	t.Parallel()
	volRoot := filepath.Join(t.TempDir(), "vol")
	_ = os.MkdirAll(volRoot, 0o755)
	baseURL, _, cleanup := newTestServerCreds(t, func(cfg *Config) {
		cfg.Volumes = []VolumeConfig{
			{Name: "main", Type: "local", Root: filepath.Join(t.TempDir(), "main")},
			{Name: "extra", Type: "local", Root: volRoot},
		}
	})
	defer cleanup()

	// PUT /s3/extra/b.txt → 落 extra 卷 user 桶。
	host := strings.TrimPrefix(baseURL, "http://")
	req, _ := http.NewRequest(http.MethodPut, baseURL+"/s3/extra/b.txt", bytes.NewReader([]byte("bucket data")))
	req.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/extra/b.txt", host, []byte("bucket data"), time.Now()))
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum([]byte("bucket data"))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	// 文件应落 extra 卷根（而非 main/默认）。
	var found string
	_ = filepath.WalkDir(volRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) == "b.txt" {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatalf("b.txt 未落 extra 卷根（%s）", volRoot)
	}

	// GET 读回。
	req2, _ := http.NewRequest(http.MethodGet, baseURL+"/s3/extra/b.txt", nil)
	req2.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodGet, "/s3/extra/b.txt", host, nil, time.Now()))
	req2.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	req2.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum(nil)))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if string(body) != "bucket data" {
		t.Fatalf("GET body = %q", body)
	}
}

// TestS3Bucket_SingleKeyDefault 单段 key → 默认卷（零回归）。
func TestS3Bucket_SingleKeyDefault(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPut, baseURL+"/s3/plain.txt", bytes.NewReader([]byte("x")))
	req.Header.Set("Authorization", sigV4SignHost(testAccessKey, testAccessSecret, http.MethodPut, "/s3/plain.txt", strings.TrimPrefix(baseURL, "http://"), []byte("x"), time.Now()))
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	req.Header.Set("x-amz-content-sha256", hex.EncodeToString(sha256sum([]byte("x"))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
}
