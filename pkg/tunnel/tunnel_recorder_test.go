// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

// tunnel_recorder_test.go 验证 streamRecorder 状态码确定性（评审 I4）：
// 流式响应改造后，加密 goroutine 在 metaReady 关闭后读取状态码——此前仅首次 Write
// 触发 metaReady，handler「先 Write body 再 WriteHeader(500)」时对端收到非确定
// 200/500（写后状态不收敛）。修复后按标准 ResponseWriter 语义：
//   - 首次 WriteHeader 或首次 Write 触发 metaReady；
//   - Write 后 WriteHeader 被忽略（状态码冻结，无竞态）。

import (
	"io"
	"net/http"
	"testing"
)

// newRecorderPipe 构造 streamRecorder + Pipe（模拟 handler 写端）。
// io.Pipe 无缓冲：Write 必须等配对 Reader 消费，否则阻塞——测试用 Discard goroutine
// 消费写端（模拟加密 goroutine 读 body），cleanup 关闭两端防 goroutine 泄漏。
func newRecorderPipe(t *testing.T) (*streamRecorder, func()) {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, pr)
	}()
	sr := newStreamRecorder(pw)
	return sr, func() {
		_ = pw.Close()
		_ = pr.Close()
		<-done
	}
}

// TestStreamRecorder_WriteHeaderFirstFreezesMeta：先 WriteHeader(500) → 状态 500 +
// metaReady 关闭（修复点：此前 WriteHeader 不触发 metaReady，加密 goroutine 会一直等）。
func TestStreamRecorder_WriteHeaderFirstFreezesMeta(t *testing.T) {
	t.Parallel()
	sr, cleanup := newRecorderPipe(t)
	defer cleanup()
	sr.WriteHeader(http.StatusInternalServerError)
	select {
	case <-sr.metaReady:
	default:
		t.Fatal("首次 WriteHeader 应触发 metaReady")
	}
	sr.mu.Lock()
	code := sr.statusCode
	sr.mu.Unlock()
	if code != http.StatusInternalServerError {
		t.Fatalf("WriteHeader(500) 后 statusCode=%d want 500", code)
	}
}

// TestStreamRecorder_WriteThenWriteHeaderIgnored：先 Write body 再 WriteHeader(500)
// → 状态冻结 200（后续 WriteHeader 被 wroteHeader 门忽略）——消除「meta 已按 200 发出、
// 状态又被改 500」的竞态。
func TestStreamRecorder_WriteThenWriteHeaderIgnored(t *testing.T) {
	t.Parallel()
	sr, cleanup := newRecorderPipe(t)
	defer cleanup()
	if _, err := sr.Write([]byte("partial body")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// handler 随后报错：标准语义下不得覆盖已写出的状态。
	sr.WriteHeader(http.StatusInternalServerError)
	select {
	case <-sr.metaReady:
	default:
		t.Fatal("首次 Write 应触发 metaReady")
	}
	sr.mu.Lock()
	code := sr.statusCode
	sr.mu.Unlock()
	if code != http.StatusOK {
		t.Fatalf("Write 后 WriteHeader(500) 应被忽略（冻结 200）, got %d", code)
	}
}

// TestStreamRecorder_WriteOnlyDefault200：只 Write 不 WriteHeader → 200（默认）。
func TestStreamRecorder_WriteOnlyDefault200(t *testing.T) {
	t.Parallel()
	sr, cleanup := newRecorderPipe(t)
	defer cleanup()
	if _, err := sr.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sr.mu.Lock()
	code := sr.statusCode
	sr.mu.Unlock()
	if code != http.StatusOK {
		t.Fatalf("只 Write 应默认 200, got %d", code)
	}
}

// TestStreamRecorder_MultipleWriteHeaderFirstWins：多次 WriteHeader → 首个生效。
func TestStreamRecorder_MultipleWriteHeaderFirstWins(t *testing.T) {
	t.Parallel()
	sr, cleanup := newRecorderPipe(t)
	defer cleanup()
	sr.WriteHeader(http.StatusCreated)
	sr.WriteHeader(http.StatusTeapot)
	sr.mu.Lock()
	code := sr.statusCode
	sr.mu.Unlock()
	if code != http.StatusCreated {
		t.Fatalf("多次 WriteHeader 应首个生效, got %d", code)
	}
}
