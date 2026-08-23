// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// tunnelTestHexKey 与 tunnel_test.go 中的 testHexKey 相同（外部包测试不能直接引用）。
var tunnelTestHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestClientDo_SendBreaksOnResponseClose 验证：HTTPClient.Do 已返回且响应被关闭时,
// body 加密 goroutine 不至于在 io.Pipe 上永久阻塞(上游断流时的防死锁兜底)。
// 服务端在读取部分请求体后立即关闭连接, 模拟上游断流。
func TestClientDo_SendBreaksOnResponseClose(t *testing.T) {
	// 服务端读取少量请求体后直接协商关闭(断流)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close() // 立即关闭连接, 不再消费
		// 不写任何响应 => HTTPClient.Do 将因连接意外关闭而返回错误
	}))
	defer ts.Close()

	client, err := tunnel.NewClient(tunnelTestHexKey, ts.URL, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/upload", strings.NewReader(strings.Repeat("x", 200000)))
	req.Header.Set("Content-Type", "multipart/form-data")

	deadline := make(chan struct{})
	go func() {
		_, _ = client.Do(req)
		close(deadline)
	}()
	select {
	case <-deadline:
		// Do 返回 => 完成(无论成功失败, 关键是 encrypt goroutine 没有挂死)
	case <-time.After(5 * time.Second):
		t.Fatal("client.Do 未在超时内返回: 请求体加密 goroutine 泄漏/死锁")
	}
}
