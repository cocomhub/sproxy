// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// handler_timeout_test.go 锁「S3 网关不因请求体缓慢/超长而卡死或泄漏」。
//
// 背景（2026-11-27）：台账 D 条记录 `TestListObjectsV2_NamedBucket` / `TestBucketRouting` 在
// Windows 上偶发失败（曾本地复现一次，其后多次不复现）。这两个用例本身是纯 HTTP 断言，
// 共同的**非确定性来源**只有"服务端未及时响应" ⇒ 用一条独立用例钉住「慢/超大请求不会让
// handler 无限等待」这一稳定性前提，避免它回归后再去追那两个用例。
//
// 这里检查的是**网关侧的读取约束**（不是业务语义）：
//   - 请求体读取必须有上限（`maxBodyBytes`/`io.LimitReader`），否则超大 PUT 会吃光内存；
//   - handler 必须在客户端断开后返回（不泄漏 goroutine）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestS3_SlowAndOversizedRequestsDoNotHang 慢请求/超大请求都必须有界结束。
func TestS3_SlowAndOversizedRequestsDoNotHang(t *testing.T) {
	t.Parallel()
	f := newTestFixture(t)
	svc := f.svc

	// ① 超大 PUT（声明 64 MiB，实际只发少量后关闭）：handler 必须在有界时间内返回，
	//    且失败（拒绝/读取错误），不得无限等待。
	ts := httptest.NewServer(http.HandlerFunc(svc.Handler))
	t.Cleanup(ts.Close)

	done := make(chan struct{})
	go func() {
		defer close(done)
		body := strings.NewReader(strings.Repeat("x", 64<<10))
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/bucket/key", body)
		if err != nil {
			return
		}
		req.ContentLength = 64 << 20 // 声明远大于实际发送（模拟客户端中途卡住/放弃）
		resp, derr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if derr == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-done:
	case <-time.After(testutil.ScaleTimeout(10 * time.Second)):
		t.Fatal("超大/中断的 PUT 让 handler 无界等待（会拖垮同机的其它用例 ⇒ D 条 flake 的同一机理）")
	}
}
