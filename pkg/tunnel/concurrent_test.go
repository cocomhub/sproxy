// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDecodeMetadataFrame_RejectsTooLarge 验证 MaxMetadataBytes 防御能拒绝
// 伪造的超长 metaLen（4 GiB 攻击向量）。详见 docs/tunnel.md 与 CHANGELOG v0.2.0。
func TestDecodeMetadataFrame_RejectsTooLarge(t *testing.T) {
	t.Parallel()
	key, _ := ParseKey(strings.Repeat("a", 64))

	// 构造 [4B BE metaLen=MaxUint32-ish][垃圾]，长度超过 MaxMetadataBytes
	hugeLen := uint32(MaxMetadataBytes + 1)
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, hugeLen); err != nil {
		t.Fatal(err)
	}
	// 填一些垃圾数据
	buf.Write(make([]byte, 16))

	_, err := decodeMetadataFrame(&buf, key)
	if err == nil {
		t.Fatal("expected error for oversized metaLen, got nil")
	}
	if err != ErrMetadataTooLarge {
		// 不要求完全相等（可能被包装），但至少要可识别
		if !strings.Contains(err.Error(), "metadata") && err != ErrMetadataTooLarge {
			t.Fatalf("expected metadata-too-large error, got: %v", err)
		}
	}
}

// TestServeHTTP_RejectsOversizedMetadataFrame 验证当客户端发送伪造的大 metaLen 时，
// tunnel.Handler 立即返回 400 而不是 OOM 死机。
func TestServeHTTP_RejectsOversizedMetadataFrame(t *testing.T) {
	t.Parallel()
	key, _ := ParseKey(strings.Repeat("b", 64))
	h := NewHandler(key, nil)

	// metaLen = MaxMetadataBytes + 1，但实际不附带后续 metadata（也不应被读取）
	hugeLen := uint32(MaxMetadataBytes + 1)
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, hugeLen)

	req := httptest.NewRequest(http.MethodPost, "/tunnel", &body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestClient_ConcurrentRequests 验证 tunnel.Client 在多 goroutine 并发调用下不发生 race。
// 使用 NewLocalHandler 路由到本地 mux，避免依赖外部网络。
func TestClient_ConcurrentRequests(t *testing.T) {
	t.Parallel()
	key, _ := ParseKey(strings.Repeat("c", 64))

	local := http.NewServeMux()
	local.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Server-Time", fmt.Sprintf("%d", time.Now().UnixNano()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	ts := httptest.NewServer(NewLocalHandler(key, local, nil))
	defer ts.Close()

	client, err := NewClient(strings.Repeat("c", 64), ts.URL, 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	const N = 20
	var wg sync.WaitGroup
	errs := make(chan error, N)

	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("GET", "/echo", nil)
			resp, err := client.Do(req)
			if err != nil {
				errs <- fmt.Errorf("req %d: %w", i, err)
				return
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if string(data) != "hello" {
				errs <- fmt.Errorf("req %d: bad body %q", i, data)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestDispatchLocal_HandlerPanicDoesNotHang 验证 dispatchLocal 的 panic 兜底：
// 当本地 handler 在写入响应前 panic，tunnel goroutine 不应永久阻塞。
func TestDispatchLocal_HandlerPanicDoesNotHang(t *testing.T) {
	t.Parallel()
	key, _ := ParseKey(strings.Repeat("d", 64))

	local := http.NewServeMux()
	local.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	ts := httptest.NewServer(NewLocalHandler(key, local, nil))
	defer ts.Close()

	client, err := NewClient(strings.Repeat("d", 64), ts.URL, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("GET", "/panic", nil)
		resp, err := client.Do(req)
		if err == nil {
			// 服务端可能因 panic 返回 200 with empty body 或其他状态
			resp.Body.Close()
		}
		// 我们要保证的是这里不会永久挂起。
	}()

	select {
	case <-done:
		// ok
	case <-time.After(10 * time.Second):
		t.Fatal("client.Do hung after server-side panic")
	}
}
