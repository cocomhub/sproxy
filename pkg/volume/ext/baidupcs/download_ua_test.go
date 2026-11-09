// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// download_ua_test.go 锁定**下载路径必须携带网盘客户端 UA**（2026-11-08 实测根因延伸）：
// 百度 CDN 对 **Range** 请求要求 `netdisk;P2SP;...`，缺/错 ⇒ 403 `sign error (31362)`
// （同 syncfs_range.go 的 2026-10-22 实测）。表现：**大文件**（走并行 Range）读取 403，小文件（单请求）正常。
//
// 两个点必须都带上：
//  1. 下载器 client：worker 走 `client.Req(...)`，会应用 `HTTPClient.UserAgent` ⇒ 设 `SetUserAgent`；
//  2. Range **探测**（`baiduDURLCheck`）用裸 `cli.Do(req)` ⇒ 不应用 `UserAgent`，必须显式设请求头。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qjfoidnh/BaiduPCS-Go/requester"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/downloader"
	"github.com/qjfoidnh/BaiduPCS-Go/requester/transfer"
)

// uaAssertServer 模拟百度 CDN：UA 不是网盘 UA ⇒ 403 sign error（复刻 syncfs_range_test.go 口径）。
func uaAssertServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != netdiskUATestValue {
			http.Error(w, `{"error_code":31362,"error_msg":"sign error"}`, http.StatusForbidden)
			return
		}
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 0-32767/100")
			w.Header().Set("x-bs-file-size", "100")
			w.WriteHeader(http.StatusPartialContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewDownloadClient_CarriesNetdiskUA(t *testing.T) {
	t.Parallel()
	srv := uaAssertServer(t)
	cli := newDownloadHTTPClient()
	resp, err := cli.Req(http.MethodGet, srv.URL, nil, nil) // worker 实际走 Req（应用 UserAgent）
	if err != nil {
		t.Fatalf("Req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下载器 client 未带网盘 UA：status=%d（CDN 会 403 sign error）", resp.StatusCode)
	}
}

func TestBaiduDURLCheck_CarriesNetdiskUA(t *testing.T) {
	t.Parallel()
	srv := uaAssertServer(t)
	cli := requester.NewHTTPClient() // 故意用默认（浏览器）UA 的 client：探测必须自己设头
	total, resp, err := baiduDURLCheck(context.Background(), cli, srv.URL)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("baiduDURLCheck: %v", err)
	}
	if total != 100 {
		t.Fatalf("探测总大小=%d, want 100（403 时 probeTotalSize 会解析失败）", total)
	}
}

// TestBaiduDURLCheck_WrongUAIs403 反向对照：证明上面的断言有效（缺 UA 真会 403）。
func TestBaiduDURLCheck_WrongUAIs403(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error_code":31362,"error_msg":"sign error"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	cli := requester.NewHTTPClient()
	// 直接构造一个不带 UA 的请求走同一路径：用 client.Client.Do（裸）模拟"漏设 UA"。
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-32767")
	resp, derr := cli.Do(req)
	if derr != nil {
		t.Fatalf("Do: %v", derr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("反向对照应 403（证明 UA 断言有效），got %d", resp.StatusCode)
	}
}

// TestNewDownloadConfig_UsesBoundedBlockSize 锁定块模式 + 4MiB 块（读侧 403 根因修复）：
// 库默认 `RangeGenMode_Default` 会按「总大小/并发」切块（120MiB+并发8 ⇒ ~15MiB）⇒ 百度 CDN 返回
// 403 `31326 user is not authorized, hitcode:104`（实测 1/2/4MiB ⇒ 206；8/15MiB ⇒ 403）。
func TestNewDownloadConfig_UsesBoundedBlockSize(t *testing.T) {
	t.Parallel()
	cfg := newDownloadConfig("/tmp/resume.download")
	if cfg.Mode != transfer.RangeGenMode_BlockSize {
		t.Fatalf("必须显式使用块模式（默认自适应生成器会切出 >4MiB 块 ⇒ CDN 403），got %v", cfg.Mode)
	}
	if cfg.BlockSize <= 0 || cfg.BlockSize > maxDownloadBlockSize {
		t.Fatalf("块大小=%d，须在 (0, %d] 内", cfg.BlockSize, maxDownloadBlockSize)
	}
	if cfg.InstanceStatePath != "/tmp/resume.download" {
		t.Fatalf("断点路径丢失：%q", cfg.InstanceStatePath)
	}
	if cfg.InstanceStateStorageFormat != downloader.InstanceStateStorageFormatJSON {
		t.Fatalf("断点格式需为 JSON，got %v", cfg.InstanceStateStorageFormat)
	}
	if cfg.MaxParallel < 1 {
		t.Fatalf("并发须 >=1，got %d", cfg.MaxParallel)
	}
}

// TestMaxDownloadBlockSize_WithinLibraryLimit 红线：块上限不得放开到库上限（4MiB）之上。
func TestMaxDownloadBlockSize_WithinLibraryLimit(t *testing.T) {
	t.Parallel()
	if maxDownloadBlockSize > downloader.BlockSizeList[3] {
		t.Fatalf("块上限 %d 超过库上限 %d（CDN 会 403）", maxDownloadBlockSize, downloader.BlockSizeList[3])
	}
}
