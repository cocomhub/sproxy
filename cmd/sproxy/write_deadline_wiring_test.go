// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
)

// deadlineRecorder 是实现了 SetWriteDeadline 的 ResponseWriter 替身：httptest.ResponseRecorder
// 不支持该能力，无法观测「滑动写截止是否真的装配」。
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	setDeadlineCalls int
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error {
	d.setDeadlineCalls++
	return nil
}

// TestCreateHTTPServer_AppliesSlidingWriteDeadline 断言装配层确实把
// server.WithSlidingWriteDeadline 挂上主 HTTP server。漏挂 ⇒ 弱网大文件下载仍会被
// server_timeouts.write（整响应挂钟）掐断，客户端表现为
// `decrypt stream: read chunk: unexpected EOF`（2026-11-07 线上复盘）。
func TestCreateHTTPServer_AppliesSlidingWriteDeadline(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		write       time.Duration
		wantApplied bool
	}{
		{name: "write_configured_applies_wrapper", write: time.Second, wantApplied: true},
		{name: "write_zero_passthrough", write: 0, wantApplied: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("stream"))
			})
			srv := createHTTPServer(&server.Config{
				ServerTimeouts: server.ServerTimeouts{Write: tc.write},
			}, handler)
			srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/download", nil))

			if body := rec.Body.String(); body != "stream" {
				t.Fatalf("响应体应原样透传，got %q", body)
			}
			if applied := rec.setDeadlineCalls > 0; applied != tc.wantApplied {
				t.Fatalf("滑动写截止装配不符：Write=%v want=%v got=%v（calls=%d）",
					tc.write, tc.wantApplied, applied, rec.setDeadlineCalls)
			}
		})
	}
}
