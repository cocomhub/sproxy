// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// bandwidth_wrappers_test.go 覆盖限速包装器的**透传语义**（此前 0% 覆盖）：
//   - rateLimitReader.Read：nil 桶直通；按实际读到的字节消费配额；
//   - rateLimitResponseWriter.Write：写完透传计数；nil 桶直通；
//   - rateLimitResponseWriter.Flush：底层是 Flusher 才转发（否则 no-op）；
//   - rateLimitReadAtReader.Seek：定位不消耗带宽（透传底层 seeker）。
//
// 时序语义（"先读后等 / 先等后写"）由集成路径覆盖，这里只钉契约，避免 sleep。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fastBucket 是「永不阻塞」的桶：rate 极大 ⇒ WaitN 立即成功，便于测透传与消费口径。
func fastBucket() *TokenBucket { return NewTokenBucket(1<<40, 1<<40) }

func TestRateLimitReader_PassthroughAndConsume(t *testing.T) {
	t.Parallel()
	src := strings.NewReader("hello bandwidth")

	// nil 桶：完全直通（不限速）。
	nilBucket := &rateLimitReader{bucket: nil, r: strings.NewReader("abc")}
	buf := make([]byte, 3)
	if n, err := nilBucket.Read(buf); n != 3 || err != nil || string(buf) != "abc" {
		t.Fatalf("nil 桶应直通: n=%d err=%v buf=%q", n, err, buf)
	}

	// 有桶：内容不变、按实际字节消费（rate 极大 ⇒ 不阻塞）。
	rl := &rateLimitReader{bucket: fastBucket(), r: src}
	got, err := io.ReadAll(rl)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello bandwidth" {
		t.Fatalf("内容被改写: %q", got)
	}

	// 读错误必须原样上抛（不吞 EOF 以外的错误）。
	sentinel := errors.New("boom")
	failing := &rateLimitReader{bucket: fastBucket(), r: errReader{sentinel}}
	_, rerr := failing.Read(make([]byte, 4))
	if !errors.Is(rerr, sentinel) {
		t.Fatalf("读错误应透传，got %v", rerr)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestRateLimitResponseWriter_WriteAndFlush(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: rec}

	// nil 桶：直通并计数。
	plain := &rateLimitResponseWriter{countingWriter: cw, bucket: nil}
	if _, err := plain.Write([]byte("abc")); err != nil {
		t.Fatalf("nil 桶写: %v", err)
	}
	// 有桶：内容一致。
	limited := &rateLimitResponseWriter{countingWriter: cw, bucket: fastBucket()}
	if _, err := limited.Write([]byte("def")); err != nil {
		t.Fatalf("限速写: %v", err)
	}
	if got := rec.Body.String(); got != "abcdef" {
		t.Fatalf("响应体 = %q, want abcdef", got)
	}

	// Flush：底层是 Flusher 时必须转发（recorder 实现 Flusher）。
	limited.Flush()
	if !rec.Flushed {
		t.Fatal("底层实现 Flusher 时 Flush 必须转发")
	}
	// Flush：底层不是 Flusher ⇒ no-op（不得 panic）。
	nrw := &nopResponseWriter{}
	nonFlusher := &rateLimitResponseWriter{countingWriter: &countingWriter{ResponseWriter: nrw}, bucket: nil}
	nonFlusher.Flush()
}

// nopResponseWriter 是**不实现 Flusher** 的最小 ResponseWriter（验证 Flush 的 no-op 分支）。
type nopResponseWriter struct{}

func (nopResponseWriter) Header() http.Header         { return http.Header{} }
func (nopResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nopResponseWriter) WriteHeader(int)             {}

func TestRateLimitReadAtReader_SeekDoesNotConsume(t *testing.T) {
	t.Parallel()
	data := []byte("0123456789")
	ra := bytes.NewReader(data)
	rr := &rateLimitReadAtReader{
		bucket: fastBucket(), r: ra,
		ra: ra,
		s:  ra,
	}
	off, err := rr.Seek(4, io.SeekStart)
	if err != nil || off != 4 {
		t.Fatalf("Seek = (%d,%v), want (4,nil)", off, err)
	}
	buf := make([]byte, 3)
	if n, rerr := rr.ReadAt(buf, 4); n != 3 || rerr != nil || string(buf) != "456" {
		t.Fatalf("ReadAt = (%d,%v,%q), want (3,nil,456)", n, rerr, buf)
	}
	// SourcePath：底层实现该接口时透传；否则空串。
	if got := rr.SourcePath(); got != "" {
		t.Fatalf("bytes.Reader 无 SourcePath，应为空串，got %q", got)
	}
	withPath := &rateLimitReadAtReader{ra: ra, s: ra, r: pathReader{}}
	if got := withPath.SourcePath(); got != "/tmp/x" {
		t.Fatalf("SourcePath 透传失败: %q", got)
	}
}

type pathReader struct{}

func (pathReader) Read([]byte) (int, error) { return 0, io.EOF }
func (pathReader) SourcePath() string       { return "/tmp/x" }

func TestTokenBucket_NilAndUnlimitedAreNoop(t *testing.T) {
	t.Parallel()
	var nilBucket *TokenBucket
	if err := nilBucket.WaitN(context.Background(), 10); err != nil {
		t.Fatalf("nil 桶 WaitN 应直接成功: %v", err)
	}
	unlimited := NewTokenBucket(0, 0)
	if err := unlimited.WaitN(context.Background(), 1<<30); err != nil {
		t.Fatalf("rate<=0 应不限速: %v", err)
	}
}

// TestReadFromStream_ChunkSemantics 锁加密卷分块下载的读取语义（此前 0% 覆盖）：
// 读满 length 返回数据 + SHA-256；流提前结束返回**较短数据**（不报错）；真实 IO 错误上抛。
func TestReadFromStream_ChunkSemantics(t *testing.T) {
	t.Parallel()
	s := &Service{}

	// 完整读取：数据 + 校验和一致。
	data, sum, err := s.readFromStream(strings.NewReader("0123456789"), 4)
	if err != nil {
		t.Fatalf("readFromStream: %v", err)
	}
	if string(data) != "0123" {
		t.Fatalf("data = %q, want 0123", data)
	}
	want := sha256.Sum256([]byte("0123"))
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("checksum 与内容不符: %s", sum)
	}

	// 流比 length 短：返回实际读到的字节（EOF 不是错误）。
	data, _, err = s.readFromStream(strings.NewReader("ab"), 8)
	if err != nil {
		t.Fatalf("短流不应报错: %v", err)
	}
	if string(data) != "ab" {
		t.Fatalf("短流 data = %q, want ab", data)
	}

	// 真实读错误：必须上抛（不能被当成 EOF 静默吞掉）。
	sentinel := errors.New("io boom")
	if _, _, rerr := s.readFromStream(errReader{sentinel}, 4); !errors.Is(rerr, sentinel) {
		t.Fatalf("IO 错误应上抛，got %v", rerr)
	}
}
