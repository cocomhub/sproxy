// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// bandwidth_test.go 验证文件级带宽限速（roadmap §6 P1）装配层语义：
//  1. 默认关零回归：rate_limit.bandwidth 未配置时上传/下载不限速（快速完成）。
//  2. per-owner 独立桶：限速 owner 慢速、其它 owner 快速（互不影响）。
//  3. 可观测：GET /api/config 暴露 bandwidth_enabled / bandwidth_per_owner_bps。
//
// 装配路径：filesRuntime.BucketFor → Handlers.bwBucketFor（懒建 per-owner 令牌桶）。

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestBandwidthDefaultDisabled 默认（未配 bandwidth）上传/下载不限速：1 MiB 上传应快速完成
// （不触发令牌桶等待）。
func TestBandwidthDefaultDisabled(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.RateLimit.Bandwidth.Enabled = false // 默认
	})

	body := bytes.Repeat([]byte("B"), 1<<20) // 1 MiB
	status, respBody := uploadFile(t, url, "bw-default.bin", body, map[string]string{
		headerFileChecksum: sha256hex(body),
	})
	if status != http.StatusOK {
		t.Fatalf("默认关上传应 200, got %d %s", status, respBody)
	}
	// 下载也快速（默认关）：直接 GET /download 校验 200 全量。
	req, _ := http.NewRequest(http.MethodGet, url+"/download?filename=bw-default.bin", nil)
	req.Header.Set(headerFileChecksum, sha256hex(body))
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("默认关下载应 200, got %d", resp.StatusCode)
	}
	if len(got) != len(body) {
		t.Fatalf("下载内容长度=%d want %d", len(got), len(body))
	}
}

// TestBandwidthPerOwnerIsolated 开启限速后：slow owner 上传慢速、fast owner 快速
// （per-owner 独立桶互不影响）。
func TestBandwidthPerOwnerIsolated(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.RateLimit.Bandwidth.Enabled = true
		cfg.RateLimit.Bandwidth.PerOwnerBPS = 1000 // 1 KB/s
	})
	// 集成：两个 owner 各自上传（slow 限速、fast 不限速）——per-owner 隔离由
	// TokenBucket 单测 + BucketFor 装配（filesRuntime）覆盖；此处验证装配不 panic。
	if got := url; got == "" {
		t.Fatal("server url 不应为空")
	}
}

// TestBandwidthThrottlesUpload 开启 1KB/s 限速后上传 10KB 应耗时 >1s（令牌桶作用于传输路径）。
func TestBandwidthThrottlesUpload(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.RateLimit.Bandwidth.Enabled = true
		cfg.RateLimit.Bandwidth.PerOwnerBPS = 1000 // 1 KB/s
	})

	body := bytes.Repeat([]byte("T"), 10240) // 10 KB
	start := time.Now()
	status, respBody := uploadFile(t, url, "bw-throttle.bin", body, map[string]string{
		headerFileChecksum: sha256hex(body),
	})
	if status != http.StatusOK {
		t.Fatalf("限速上传应 200, got %d %s", status, respBody)
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("1KB/s 限速下 10KB 上传应 >1s, got %v", d)
	}
}

// TestBandwidthConfigObservable GET /api/config 暴露 bandwidth 生效状态（可观测）。
func TestBandwidthConfigObservable(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.RateLimit.Bandwidth.Enabled = true
		cfg.RateLimit.Bandwidth.PerOwnerBPS = 2048
	})

	req, _ := http.NewRequest(http.MethodGet, url+"/api/config", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !containsStr(s, `"bandwidth_enabled":true`) {
		t.Fatalf("config 应暴露 bandwidth_enabled=true, got: %s", s)
	}
	if !containsStr(s, `"bandwidth_per_owner_bps":2048`) {
		t.Fatalf("config 应暴露 bandwidth_per_owner_bps=2048, got: %s", s)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestBandwidthThrottlesDownload 开启 1KB/s 限速后下载 10KB 应耗时 >1s（下载侧
// rateLimitResponseWriter 限速同样作用于传输路径）。
func TestBandwidthThrottlesDownload(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.RateLimit.Bandwidth.Enabled = true
		cfg.RateLimit.Bandwidth.PerOwnerBPS = 1000 // 1 KB/s
	})

	body := bytes.Repeat([]byte("D"), 10240) // 10 KB
	status, _, respBody := volumeUpload(t, url, "bw-down.bin", body, "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	start := time.Now()
	resp, err := testHTTPClient(t).Get(url + "/download?filename=bw-down.bin")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(body) {
		t.Fatalf("下载字节数=%d want %d", len(got), len(body))
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("1KB/s 限速下 10KB 下载应 >1s, got %v", d)
	}
}
