// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// ---- Option functions ----

func TestWithHTTPClient(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	hc := &http.Client{Timeout: 99 * time.Second}
	WithHTTPClient(hc)(c)
	if c.httpClient.Timeout != 99*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 99s", c.httpClient.Timeout)
	}
}

func TestWithTimeout(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithTimeout(123 * time.Second)(c)
	if c.httpClient.Timeout != 123*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 123s", c.httpClient.Timeout)
	}
}

func TestWithMaxChunkSize(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithMaxChunkSize(8888)(c)
	if c.maxChunkSize != 8888 {
		t.Errorf("MaxChunkSize = %d, want 8888", c.maxChunkSize)
	}
}

func TestWithAuthToken(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithAuthToken("my-token")(c)
	if c.authToken != "my-token" {
		t.Errorf("authToken = %q, want %q", c.authToken, "my-token")
	}
}

func TestWithAuthToken_Empty(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithAuthToken("")(c)
	if c.authToken != "" {
		t.Errorf("authToken should be empty, got %q", c.authToken)
	}
}

func TestWithLogger(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	WithLogger(logger)(c)
	if c.logger != logger {
		t.Error("WithLogger did not set the logger")
	}
}

func TestWithLogger_Nil(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithLogger(nil)(c)
	if c.logger == nil {
		t.Error("WithLogger(nil) should keep the default logger")
	}
}

func TestWithTunnel_ValidKey(t *testing.T) {
	t.Parallel()

	c := NewFileClient("http://127.0.0.1:18083")
	// 64 hex chars = 32 bytes = valid AES-256 key
	validKey := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	WithTunnel(validKey)(c)
	if c.tunnelClient == nil {
		t.Fatal("tunnelClient should not be nil for valid key")
	}
}

func TestWithTunnel_InvalidKey(t *testing.T) {
	t.Parallel()

	c := NewFileClient("http://127.0.0.1:18083")
	WithTunnel(strings.Repeat("abcdef", 11))(c) // 66 chars → invalid, logged as warn
	if c.tunnelClient != nil {
		t.Fatal("tunnelClient should be nil for invalid key")
	}
}

func TestWithProgress(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	var called atomic.Int64
	fn := func(_ string, read, _ int64) {
		called.Add(read)
	}
	WithProgress(fn)(c)
	if c.progressFn == nil {
		t.Fatal("progressFn should be set")
	}
	// 手动调用
	c.progressFn("test", 42, 100)
	if called.Load() != 42 {
		t.Errorf("progress called with %d, want 42", called.Load())
	}
}

// ---- ProgressReader ----

func TestNewProgressReader(t *testing.T) {
	var called bool
	pr := NewProgressReader(strings.NewReader("hello"), 5, func(read, total int64) {
		called = true
		if read != 5 || total != 5 {
			t.Errorf("unexpected read=%d total=%d", read, total)
		}
	})
	buf := make([]byte, 10)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 5 {
		t.Errorf("Read returned %d, want 5", n)
	}
	if !called {
		t.Error("progress callback not called")
	}
}

func TestProgressReader_NilCallback(t *testing.T) {
	pr := NewProgressReader(strings.NewReader("hi"), 2, nil)
	buf := make([]byte, 10)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 2 {
		t.Errorf("Read returned %d, want 2", n)
	}
}

func TestProgressReader_EOF(t *testing.T) {
	var totalRead int64
	pr := NewProgressReader(strings.NewReader("abc"), 3, func(read, _ int64) {
		totalRead = read
	})
	buf := make([]byte, 10)
	_, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	_, err = pr.Read(buf)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	// EOF 时 totalRead 应保持不变
	if totalRead != 3 {
		t.Errorf("totalRead = %d, want 3", totalRead)
	}
}

// ---- FormatByte / FormatETA ----

// (moved to format_test.go)
// ---- ChunkedOption functions ----

func TestWithChunkedChunkSize(t *testing.T) {
	o := &chunkedOpts{}
	WithChunkedChunkSize(9999)(o)
	if o.chunkSize != 9999 {
		t.Errorf("chunkSize = %d, want 9999", o.chunkSize)
	}
}

func TestWithChunkedConcurrency(t *testing.T) {
	o := &chunkedOpts{}
	WithChunkedConcurrency(7)(o)
	if o.concurrency != 7 {
		t.Errorf("concurrency = %d, want 7", o.concurrency)
	}
}

func TestWithChunkedResume(t *testing.T) {
	o := &chunkedOpts{}
	WithChunkedResume(false)(o)
	if o.resume {
		t.Error("resume should be false")
	}
}

// ---- Missing Option functions ----

func TestWithChunkSize(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithChunkSize(8888)(c)
	if c.chunkSize != 8888 {
		t.Errorf("chunkSize = %d, want 8888", c.chunkSize)
	}
}

func TestWithChunkSize_Zero(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithChunkSize(0)(c)
	if c.chunkSize != 0 {
		t.Errorf("chunkSize should be 0 when passed 0, got %d", c.chunkSize)
	}
}

func TestWithCacheOptions(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	WithCacheOptions(500, 5*time.Minute)(c)
	if c.maxCacheEntries != 500 {
		t.Errorf("maxCacheEntries = %d, want 500", c.maxCacheEntries)
	}
	if c.cacheTTL != 5*time.Minute {
		t.Errorf("cacheTTL = %v, want 5m", c.cacheTTL)
	}
}

func TestWithCacheOptions_ZeroValues(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	origMax := c.maxCacheEntries
	origTTL := c.cacheTTL
	WithCacheOptions(0, 0)(c)
	if c.maxCacheEntries != origMax {
		t.Errorf("maxCacheEntries should remain %d, got %d", origMax, c.maxCacheEntries)
	}
	if c.cacheTTL != origTTL {
		t.Errorf("cacheTTL should remain %v, got %v", origTTL, c.cacheTTL)
	}
}

func TestWithKVStore(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	store := NewMemoryKVStore()
	WithKVStore(store)(c)
	if c.chainManager == nil {
		t.Fatal("expected chainManager to be set")
	}
}

func TestWithCacheDir(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	dir := t.TempDir()
	WithCacheDir(dir)(c)
	if c.chainManager == nil {
		t.Fatal("expected chainManager to be set with valid dir")
	}
}

func TestWithCacheDir_InvalidDir(t *testing.T) {
	// 使用一个已存在的文件路径作为"目录"（会失败，降级为内存存储而非 panic）
	existingFile := filepath.Join(t.TempDir(), "existing_file")
	if err := os.WriteFile(existingFile, []byte("not a dir"), 0644); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient("http://127.0.0.1:18083",
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithCacheDir(existingFile),
	)
	if c.chainManager == nil {
		t.Fatal("expected chainManager to be set (fallback to memory store)")
	}
}

// ---- closeBodyIfErr ----

func TestCloseBodyIfErr_NoError(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("ok"))}
	r, err := closeBodyIfErr(resp, nil)
	if r != resp {
		t.Error("should return resp unchanged")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCloseBodyIfErr_WithNilBody(t *testing.T) {
	r, err := closeBodyIfErr(&http.Response{Body: nil}, nil)
	if r == nil {
		t.Error("should return resp even with nil body")
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCloseBodyIfErr_ErrorWithBody(t *testing.T) {
	body := io.NopCloser(strings.NewReader("should be closed"))
	resp := &http.Response{Body: body}
	r, err := closeBodyIfErr(resp, io.ErrUnexpectedEOF)
	if r != nil {
		t.Error("should return nil resp on error")
	}
	if err != io.ErrUnexpectedEOF {
		t.Errorf("wanted ErrUnexpectedEOF, got %v", err)
	}
}

// ---- Mkdir / Rmdir ----

func TestMkdir_RoundTrip(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	// 添加 mkdir 路由
	ts.Config.Handler.(*http.ServeMux).HandleFunc("POST /mkdir", func(w http.ResponseWriter, r *http.Request) {
		dirname := r.URL.Query().Get("dirname")
		if dirname == "" {
			http.Error(w, "missing dirname", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
	})

	c := NewFileClient(ts.URL)
	if err := c.Mkdir(t.Context(), "testdir"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_ = dir // 引用避免编译错误
}

func TestMkdir_ServerError(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	ts.Config.Handler.(*http.ServeMux).HandleFunc("POST /mkdir", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"success":false,"message":"bad"}`, http.StatusBadRequest)
	})

	c := NewFileClient(ts.URL)
	if err := c.Mkdir(t.Context(), "bad"); err == nil {
		t.Fatal("expected error for server failure")
	}
}

func TestRmdir_RoundTrip(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	ts.Config.Handler.(*http.ServeMux).HandleFunc("POST /rmdir", func(w http.ResponseWriter, r *http.Request) {
		dirname := r.URL.Query().Get("dirname")
		if dirname == "" {
			http.Error(w, "missing dirname", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
	})

	c := NewFileClient(ts.URL)
	if err := c.Rmdir(t.Context(), "testdir"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}
}

func TestRmdir_ServerError(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	ts.Config.Handler.(*http.ServeMux).HandleFunc("POST /rmdir", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"success":false,"message":"not found"}`, http.StatusNotFound)
	})

	c := NewFileClient(ts.URL)
	if err := c.Rmdir(t.Context(), "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent dir")
	}
}

// ---- TunnelDo ----

func TestTunnelDo_WithoutTunnel(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:18083")
	req, _ := http.NewRequest("GET", "/test", nil)
	_, err := c.TunnelDo(req)
	if err == nil {
		t.Fatal("expected tunnel not configured error")
	}
}

// ---- LoadFromProvider (config) ----
// Note: LoadFromProvider is tested via config_test.go pattern already.

// ---- WithXfer Tests ----

func TestWithXferSetsName(t *testing.T) {
	c := &FileClient{logger: testLogger()}
	opt := WithXfer("ws", "ws://hub:8080/ws", "")
	opt(c)
	if c.xferName != "ws" {
		t.Fatalf("expected xferName ws, got %q", c.xferName)
	}
	if c.hubURL != "ws://hub:8080/ws" {
		t.Fatalf("expected hubURL ws://hub:8080/ws, got %q", c.hubURL)
	}
	if c.tunnelKey != nil {
		t.Fatal("expected nil tunnelKey for empty hexKey")
	}
}

func TestWithXferWithKey(t *testing.T) {
	c := &FileClient{logger: testLogger()}
	opt := WithXfer("ws", "ws://hub:8080/ws", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	opt(c)
	if c.tunnelKey == nil {
		t.Fatal("expected non-nil tunnelKey")
	}
	if len(c.tunnelKey) != 32 {
		t.Fatalf("expected 32 bytes key, got %d", len(c.tunnelKey))
	}
}

func TestWithXferInvalidKey(t *testing.T) {
	c := &FileClient{logger: testLogger()}
	opt := WithXfer("ws", "ws://hub:8080/ws", "bad-key")
	opt(c)
	if c.tunnelKey != nil {
		t.Fatal("expected nil tunnelKey for invalid hex")
	}
}

func TestTunnelDo_WithXferNoTransport(t *testing.T) {
	// WithXfer 设置了 name 但传输层未注册，getTunnelMux 应返回错误
	c := &FileClient{
		serverURL: "http://127.0.0.1:18083",
		xferName:  "nonexistent",
		hubURL:    "ws://hub:8080/ws",
		logger:    testLogger(),
	}
	req, _ := http.NewRequest("GET", "/test", nil)
	_, err := c.TunnelDo(req)
	if err == nil {
		t.Fatal("expected error for unregistered xfer transport")
	}
}

func TestTunnelDo_WithTunnel(t *testing.T) {
	// WithTunnel 被 WithXfer 抢占 — 预期 xfer 错误（因 ws 未注册）
	c := &FileClient{
		serverURL:  "http://127.0.0.1:18083",
		httpClient: http.DefaultClient,
		xferName:   "ws",
		logger:     testLogger(),
	}
	req, _ := http.NewRequest("GET", "/test", nil)
	_, err := c.TunnelDo(req)
	if err == nil {
		t.Fatal("expected error for unregistered xfer transport")
	}
}

// testLogger 返回写入 discard 的日志器，避免测试输出混乱。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- E2E: xfer Pipe + mux + Tunnel ----

// waitForTunnel 轮询等待 tunnel 服务就绪，替代 flaky time.Sleep。
func waitForTunnel(t *testing.T, tun *tunnel.Tunnel, ctx context.Context) {
	t.Helper()
	for range 10 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		_, err := tun.Do(req)
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tunnel not ready after 100ms")
}

func TestXferTunnelRoundTrip(t *testing.T) {
	// 端到端测试：用 xfertest.Pipe 模拟传输层，
	// 通过 mux -> Tunnel.Do/Serve 完成一个完整的 HTTP 请求-响应往返
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	t.Cleanup(func() { muxA.Close() })
	t.Cleanup(func() { muxB.Close() })

	tunA := tunnel.NewTunnel(muxA, nil)
	tunB := tunnel.NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	waitForTunnel(t, tunA, ctx)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/echo", strings.NewReader("e2e"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "e2e" {
		t.Fatalf("expected %q, got %q", "e2e", string(body))
	}
	cancel()
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		t.Error("tunB.Serve did not exit after cancel")
	}
}

func TestXferTunnelConcurrentStreams(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	t.Cleanup(func() { muxA.Close() })
	t.Cleanup(func() { muxB.Close() })

	tunA := tunnel.NewTunnel(muxA, nil)
	tunB := tunnel.NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(r.Method))
		}))
	}()
	waitForTunnel(t, tunA, ctx)

	// 并发 10 个请求
	errCh := make(chan error, 10)
	for range 10 {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			resp, err := tunA.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			errCh <- nil
		}()
	}

	for range 10 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		t.Error("tunB.Serve did not exit after cancel")
	}
}

func TestXferTunnelEncrypted(t *testing.T) {
	key, err := tunnel.ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	t.Cleanup(func() { muxA.Close() })
	t.Cleanup(func() { muxB.Close() })

	tunA := tunnel.NewTunnel(muxA, key)
	tunB := tunnel.NewTunnel(muxB, key)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(bytes.ToUpper(body))
		}))
	}()
	waitForTunnel(t, tunA, ctx)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/enc", strings.NewReader("test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "TEST" {
		t.Fatalf("expected TEST, got %q", string(body))
	}
	cancel()
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		t.Error("tunB.Serve did not exit after cancel")
	}
}

func TestXferTunnelLargeBody(t *testing.T) {
	// mux 帧最大负载 65535，测试体必须小于等于该值
	payload := strings.Repeat("A", 65000)
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	t.Cleanup(func() { muxA.Close() })
	t.Cleanup(func() { muxB.Close() })

	tunA := tunnel.NewTunnel(muxA, nil)
	tunB := tunnel.NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			w.Write(bytes.ToUpper(b))
		}))
	}()
	waitForTunnel(t, tunA, ctx)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/big", strings.NewReader(payload))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 65000 {
		t.Fatalf("expected %d bytes, got %d", len(payload), len(body))
	}
	cancel()
	select {
	case <-srvErr:
	case <-time.After(2 * time.Second):
		t.Error("tunB.Serve did not exit after cancel")
	}
}
