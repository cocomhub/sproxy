// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestWithSlidingWriteDeadline_StreamOutlivesServerWriteTimeout 是线上故障回归：
// http.Server.WriteTimeout 是**整响应挂钟**上限 ⇒ 弱网大文件下载必然超过它，服务端
// 在响应中途关连接，隧道客户端只读到半个帧并报 `decrypt stream: read chunk:
// unexpected EOF`（多个并发下载会几乎同时集体失败）。
//
// 两个子用例：
//   - control：不包装 ⇒ 必须在 grace 后被截断（证明夹具确实命中 WriteTimeout，不是空转）；
//   - fixed：包装后只要每步都有写进展，响应总时长可远超 WriteTimeout 而完整送达。
func TestWithSlidingWriteDeadline_StreamOutlivesServerWriteTimeout(t *testing.T) {
	t.Parallel()
	const (
		grace    = 250 * time.Millisecond
		rounds   = 10
		perRound = 50 * time.Millisecond // 每步进展 << grace，且总时长（500ms）= 2×grace
	)
	payload := strings.Repeat("0123456789", rounds)

	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		for i := range rounds {
			if _, err := io.WriteString(w, payload[i*10:(i+1)*10]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// 有意计时：制造「响应总时长 > WriteTimeout，但每一步都有写进展」的流式响应
			// （无条件轮询可替代的实时语义；见 internal/archcheck sleep 棘轮登记）。
			time.Sleep(perRound)
		}
	}

	t.Run("control_truncates_without_middleware", func(t *testing.T) {
		t.Parallel()
		body, err := fetchWithWriteTimeout(t, grace, handler)
		if err == nil && len(body) == len(payload) {
			t.Fatalf("夹具失效：未命中 WriteTimeout 截断（got %d bytes, err=%v）", len(body), err)
		}
	})

	t.Run("middleware_keeps_stream_alive", func(t *testing.T) {
		t.Parallel()
		wrapped := WithSlidingWriteDeadline(http.HandlerFunc(handler), grace)
		body, err := fetchWithWriteTimeout(t, grace, wrapped.ServeHTTP)
		if err != nil {
			t.Fatalf("持续有写进展的流式响应应完整送达（滑动写截止），实测 err=%v（已读 %d/%d 字节）",
				err, len(body), len(payload))
		}
		if body != payload {
			t.Fatalf("响应体不完整：got %d bytes want %d", len(body), len(payload))
		}
	})
}

// TestWithSlidingWriteDeadline_NoGraceIsPassthrough：WriteTimeout 未配置（<=0）时
// 不包装（无行为变化，避免无意义的每写一次 SetWriteDeadline）。
func TestWithSlidingWriteDeadline_NoGraceIsPassthrough(t *testing.T) {
	t.Parallel()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if got := WithSlidingWriteDeadline(h, 0); got == nil {
		t.Fatal("grace<=0 应原样返回 handler（非 nil）")
	}
	if got := WithSlidingWriteDeadline(nil, time.Second); got != nil {
		t.Fatal("nil handler 应原样返回 nil")
	}
}

// fetchWithWriteTimeout 起一个 WriteTimeout=writeTimeout 的真实 HTTP 服务并读取整个响应体。
// 返回 (body, readErr)：控制组应拿到 readErr（截断），修复组应为 nil。
func fetchWithWriteTimeout(t *testing.T, writeTimeout time.Duration, h http.HandlerFunc) (string, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, WriteTimeout: writeTimeout}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// 硬规则：测试网络客户端必须隔离连接池，不得用 http.DefaultClient；亦不得裸构造
	// Transport（R19 门禁）——统一用 netutil.IsolatedTransport() 基座。
	client := &http.Client{Transport: netutil.IsolatedTransport()}
	resp, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	return string(b), rerr
}
