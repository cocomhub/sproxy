// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestClient_WithNoTimeoutSkipsClientTimeout 验证 WithNoTimeout 让隧道请求跳过
// HTTPClient 的整请求 Timeout：长时流式响应（下载）不应被客户端超时掐断。
func TestClient_WithNoTimeoutSkipsClientTimeout(t *testing.T) {
	t.Parallel()
	body := "slow-tunnel-body"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		started <- struct{}{}
		<-release // 挂起响应体，直到测试确认已超过客户端级 Timeout
		_, _ = w.Write([]byte(body))
	}))
	defer backend.Close()

	handler := withTunnelKey(testKey, NewLocalHandler(nil, nil, nil))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client, err := NewClient(testHexKey, ts.URL, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	req, err := http.NewRequest("GET", backend.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(WithNoTimeout(req.Context()))

	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, derr := client.Do(req)
		if derr != nil {
			done <- result{err: derr}
			return
		}
		defer resp.Body.Close()
		b, rerr := io.ReadAll(resp.Body)
		done <- result{body: b, err: rerr}
	}()
	<-started
	// 持续超过客户端级 Timeout（50ms）后再放行：若未跳过整请求超时，早已失败。
	begin := time.Now()
	testutil.WaitFor(t, 5*time.Second, func() bool { return time.Since(begin) >= 200*time.Millisecond },
		"等待超过隧道客户端级超时窗口")
	close(release)

	res := <-done
	if res.err != nil {
		t.Fatalf("读取慢响应失败（整请求超时未被跳过）: %v", res.err)
	}
	if string(res.body) != body {
		t.Fatalf("响应体不符：got %q, want %q", string(res.body), body)
	}
}
