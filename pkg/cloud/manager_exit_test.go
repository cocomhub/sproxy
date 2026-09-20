// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// TestCloudManager_ExitDial_InjectsTransport 验证 ExitDial 注入后下载器
// httpClient.Transport.DialContext 被覆写（经 mesh 出口拨号路径）。
func TestCloudManager_ExitDial_InjectsTransport(t *testing.T) {
	t.Parallel()
	cfg := CloudDownloadConfig{
		MaxConcurrent: 1,
		MaxBatchURLs:  10,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
		MaxRetries:    1,
		RetryDelay:    time.Millisecond,
		Downloader:    "http",
	}
	called := false
	cfg.ExitDial = func(ctx context.Context, addr string) (net.Conn, error) {
		called = true
		return nil, net.ErrClosed // 桩：证明被调用（连接建立由真实 mesh 负责，单测只验注入）
	}
	mgr, _ := newCloudTestManager(t, t.TempDir(), nil, &cfg)
	if mgr == nil {
		t.Fatalf("manager 构造失败")
	}
	// 验证下载器 Transport.DialContext 已被覆写：直接调用下载器 httpClient 拨号。
	dl, ok := mgr.dl.(*downloader.HTTPDownloader)
	if !ok {
		t.Skipf("下载器不是 HTTPDownloader（%T），跳过", mgr.dl)
	}
	// 经下载器内部 httpClient 拨号（通过一个假请求路径验证 DialContext 生效）。
	// 直接断言 Transport.DialContext 被注入：构造拨号调用。
	tr, ok := dl.HTTPClient().Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Transport 非 *http.Transport: %T", dl.HTTPClient().Transport)
	}
	_, _ = tr.DialContext(context.Background(), "tcp", "example.com:443")
	if !called {
		t.Fatalf("ExitDial 未被调用（DialContext 未覆写）")
	}
}
