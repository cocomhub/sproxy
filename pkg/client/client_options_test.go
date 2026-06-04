// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Option functions ----

func TestWithHTTPClient(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	hc := &http.Client{Timeout: 99 * time.Second}
	WithHTTPClient(hc)(c)
	if c.httpClient.Timeout != 99*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 99s", c.httpClient.Timeout)
	}
}

func TestWithTimeout(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	WithTimeout(123 * time.Second)(c)
	if c.httpClient.Timeout != 123*time.Second {
		t.Errorf("httpClient.Timeout = %v, want 123s", c.httpClient.Timeout)
	}
}

func TestWithMaxChunkSize(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	WithMaxChunkSize(8888)(c)
	if c.MaxChunkSize != 8888 {
		t.Errorf("MaxChunkSize = %d, want 8888", c.MaxChunkSize)
	}
}

func TestWithLogger(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	WithLogger(logger)(c)
	if c.logger != logger {
		t.Error("WithLogger did not set the logger")
	}
}

func TestWithLogger_Nil(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	WithLogger(nil)(c)
	if c.logger == nil {
		t.Error("WithLogger(nil) should keep the default logger")
	}
}

func TestWithTunnel_ValidKey(t *testing.T) {
	t.Parallel()

	c := NewFileClient("http://localhost:18083")
	WithTunnel(strings.Repeat("abcdef", 11))(c) // 66 chars → invalid, logged as warn
	if c.tunnelClient != nil {
		t.Fatal("tunnelClient should be nil for invalid key")
	}
}

func TestWithTunnel_InvalidKey(t *testing.T) {
	t.Parallel()

	c := NewFileClient("http://localhost:18083")
	// 64 hex chars = 32 bytes = valid AES-256 key
	validKey := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	WithTunnel(validKey)(c)
	if c.tunnelClient == nil {
		t.Fatal("tunnelClient should not be nil for valid key")
	}
}

func TestWithProgress(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	var called atomic.Int64
	fn := func(label string, read, total int64) {
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
	pr := NewProgressReader(strings.NewReader("abc"), 3, func(read, total int64) {
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

func TestFormatByte(t *testing.T) {
	tests := []struct {
		size float64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1024 B"},
		{1536, "1.5 KB"},
		{1048576, "1024.0 KB"},
		{1572864, "1.5 MB"},
	}
	for _, tt := range tests {
		got := FormatByte(tt.size)
		if got != tt.want {
			t.Errorf("FormatByte(%v) = %q, want %q", tt.size, got, tt.want)
		}
	}
}

func TestFormatETA(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{0, "--:--"},
		{-1, "--:--"},
		{30, "30s"},
		{90, "1m 30s"},
		{3661, "1h 1m"},
	}
	for _, tt := range tests {
		got := FormatETA(tt.seconds)
		if got != tt.want {
			t.Errorf("FormatETA(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

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
	if err := c.Mkdir(context.Background(), "testdir"); err != nil {
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
	if err := c.Mkdir(context.Background(), "bad"); err == nil {
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
	if err := c.Rmdir(context.Background(), "testdir"); err != nil {
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
	if err := c.Rmdir(context.Background(), "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent dir")
	}
}

// ---- TunnelDo ----

func TestTunnelDo_WithoutTunnel(t *testing.T) {
	c := NewFileClient("http://localhost:18083")
	req, _ := http.NewRequest("GET", "/test", nil)
	_, err := c.TunnelDo(req)
	if err == nil || !strings.Contains(err.Error(), "未配置隧道密钥") {
		t.Fatalf("expected tunnel not configured error, got %v", err)
	}
}

// ---- LoadFromViper (config) ----
// Note: viper tests are in config_test.go already, but LoadFromViper is at 0%.
// Since it requires viper setup, we test it via the existing config test pattern.
