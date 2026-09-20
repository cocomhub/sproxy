// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestServeDialAuditLog 断言出口拨号审计日志含结构化字段：addr（目标地址）、
// path（路径类型 via-relay/via-direct/direct）、remote（出口连接对端）。
// TDD 红灯：现 leaf.go 日志无 path 字段。并行安全：logger 写 buf 与轮询读
// buf 之间用 mutex 串行化（日志写入 goroutine 并发）。
func TestServeDialAuditLog(t *testing.T) {
	t.Parallel()

	// 出口目标：回环 echo 服务器（dialPolicy 显式放行）。
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, r.Body)
	}))
	defer echo.Close()
	targetAddr := strings.TrimPrefix(echo.URL, "http://")

	// capture logger：dOK 分支日志写入 buf（mu 串行化读写，防 -race）。
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&syncWriter{mu: &mu, buf: &buf}, nil))

	// 双端 mux：Serve 端（Listener）+ 调用方端（Dialer）。
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Serve 端：放行一切的自定义 dialPolicy。
	dialPolicy := func(addr string) (string, bool) { return addr, true }
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(ctx, serverMux, "http://127.0.0.1:1", true,
			&http.Client{Transport: &http.Transport{}}, logger,
			ServeOptions{DialPolicy: dialPolicy})
	}()

	// 调用方端：开流写 dial 帧（含 Path="via-relay" 模拟经中间节点出口）。
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	head, merr := json.Marshal(hub.DialRequest{Dial: targetAddr, AwaitResult: true, Path: "via-relay"})
	if merr != nil {
		t.Fatal(merr)
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(head); werr != nil {
		t.Fatal(werr)
	}

	// 等待出口拨号成功日志出现（R14：用 WaitFor 而非手写 time.Sleep 轮询）。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		ok := strings.Contains(buf.String(), "出口拨号成功")
		mu.Unlock()
		return ok
	}, "出口拨号成功日志未出现")

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	for _, want := range []string{"出口拨号成功", "addr=" + targetAddr, "path=via-relay", "remote="} {
		if !strings.Contains(got, want) {
			t.Fatalf("审计日志缺 %q；完整日志:\n%s", want, got)
		}
	}
}

// syncWriter 是并发安全的 io.Writer（mu 保护 buf）。
type syncWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// TestDialAuditPath 断言 dialAuditPath 的路径类型判定：
// Path 显式优先；为空回落 AwaitResult（带 = via，无 = direct）。
func TestDialAuditPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    hub.DialRequest
		want string
	}{
		{"via-relay explicit", hub.DialRequest{Dial: "x:1", Path: "via-relay"}, "via-relay"},
		{"via-direct explicit", hub.DialRequest{Dial: "x:1", Path: "via-direct"}, "via-direct"},
		{"await-result fallback", hub.DialRequest{Dial: "x:1", AwaitResult: true}, "via"},
		{"plain direct", hub.DialRequest{Dial: "x:1"}, "direct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialAuditPath(tc.d); got != tc.want {
				t.Fatalf("dialAuditPath(%+v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestServeDialAuditLog_FailurePath 断言出口拨号**失败**路径的审计日志也含
// path 字段（P2-3）：拨不可达地址（127.0.0.1:1 无监听）→ 失败 Warn 含
// path（归因路径类型）与 dial（解析地址）。
func TestServeDialAuditLog_FailurePath(t *testing.T) {
	t.Parallel()

	// capture logger：mu 串行化读写。
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&syncWriter{mu: &mu, buf: &buf}, nil))

	// 双端 mux。
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 放行一切（不可达地址也允许尝试拨号，由 net.DialTimeout 失败）。
	dialPolicy := func(addr string) (string, bool) { return addr, true }
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(ctx, serverMux, "http://127.0.0.1:1", true,
			&http.Client{Transport: &http.Transport{}}, logger,
			ServeOptions{DialPolicy: dialPolicy})
	}()

	// 不可达地址：127.0.0.1:1（通常无监听，拨号立即拒绝）。
	unreachable := "127.0.0.1:1"
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	head, merr := json.Marshal(hub.DialRequest{Dial: unreachable, AwaitResult: true, Path: "via-direct"})
	if merr != nil {
		t.Fatal(merr)
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(head); werr != nil {
		t.Fatal(werr)
	}

	// 等待失败日志出现（WaitFor，R14 合规）。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		ok := strings.Contains(buf.String(), "出口拨号失败")
		mu.Unlock()
		return ok
	}, "出口拨号失败日志未出现")

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	for _, want := range []string{"出口拨号失败", "path=via-direct", "dial=" + unreachable} {
		if !strings.Contains(got, want) {
			t.Fatalf("失败审计日志缺 %q；完整日志:\n%s", want, got)
		}
	}
}
