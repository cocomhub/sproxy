// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package iocopy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCopyWithContext_PlainCopy(t *testing.T) {
	t.Parallel()
	var dst bytes.Buffer
	n, err := CopyWithContext(context.Background(), &dst, strings.NewReader("hello-copy"))
	if err != nil || n != 10 || dst.String() != "hello-copy" {
		t.Fatalf("CopyWithContext = (%d,%v,%q), want (10,nil,hello-copy)", n, err, dst.String())
	}
}

// TestCopyWithContext_CancelMidStream 锁取消语义：ctx 取消后不再继续读，
// 且返回**已写入**的字节数（调用方据此回滚/记账）。
func TestCopyWithContext_CancelMidStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	src := &cancelAfterReader{cancel: cancel, remaining: 4 << 20}
	var dst bytes.Buffer
	n, err := CopyWithContext(ctx, &dst, src)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != int64(dst.Len()) {
		t.Fatalf("返回值 %d 与实际写入 %d 不一致", n, dst.Len())
	}
	if dst.Len() >= 4<<20 {
		t.Fatalf("取消后仍在整份拷贝（写了 %d 字节）", dst.Len())
	}
}

// cancelAfterReader 第一次 Read 后取消 ctx（模拟客户端断开）。
type cancelAfterReader struct {
	cancel    context.CancelFunc
	remaining int
	done      bool
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	if !r.done {
		r.done = true
		r.cancel()
	}
	return n, nil
}

func TestCopyWithContext_WriteAndReadErrors(t *testing.T) {
	t.Parallel()
	wErr := errors.New("write boom")
	if _, err := CopyWithContext(context.Background(), errWriter{wErr}, strings.NewReader("abc")); !errors.Is(err, wErr) {
		t.Fatalf("写错误应上抛，got %v", err)
	}
	rErr := errors.New("read boom")
	if _, err := CopyWithContext(context.Background(), io.Discard, errReader{rErr}); !errors.Is(err, rErr) {
		t.Fatalf("读错误应上抛，got %v", err)
	}
}

type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
