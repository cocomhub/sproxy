// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

// TestSanitizeRelPath 覆盖路径清洗的安全边界。
//
// 这个函数是 `pkg/sync` 两个 FS 实现（LocalFS / HTTPTransport）的**唯一**路径入口，
// 一旦漏判就是把同步目标写出到预期目录之外，故按「拒绝清单」逐条钉住。
func TestSanitizeRelPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          string
		want        string
		wantErr     bool
		windowsOnly bool
	}{
		{name: "空串表示根", in: "", want: ""},
		{name: "普通相对路径", in: "a/b.txt", want: "a/b.txt"},
		{name: "单层文件名", in: "f.txt", want: "f.txt"},
		{name: "冗余点段被规范化", in: "a/./b", want: "a/b"},
		{name: "中间向上段被正常消解", in: "a/../b", want: "b"},
		{name: "末尾斜杠被去掉", in: "a/b/", want: "a/b"},
		{name: "点路径非法", in: ".", wantErr: true},
		{name: "向上穿越拒绝", in: "..", wantErr: true},
		{name: "以向上段开头拒绝", in: "../x", wantErr: true},
		{name: "规范化后逃出根拒绝", in: "a/../../b", wantErr: true},
		{name: "绝对路径拒绝", in: "/abs", wantErr: true},
		{name: "空字节拒绝", in: "a\x00b", wantErr: true},
		{name: "Windows 反斜杠归一", in: `a\b`, want: "a/b", windowsOnly: true},
		{name: "Windows 反斜杠开头视为绝对路径", in: `\a\b`, wantErr: true, windowsOnly: true},
		{name: "Windows 反斜杠穿越拒绝", in: `..\x`, wantErr: true, windowsOnly: true},
		{name: "Windows 盘符拒绝", in: "C:/x", wantErr: true, windowsOnly: true},
		{name: "Windows 非法字符拒绝", in: "a<b", wantErr: true, windowsOnly: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.windowsOnly && runtime.GOOS != "windows" {
				t.Skip("仅 Windows 语义")
			}
			got, err := SanitizeRelPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SanitizeRelPath(%q) 应报错，实际返回 %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SanitizeRelPath(%q) 意外报错: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("SanitizeRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// errReader 先返回一段数据再返回固定错误（覆盖「读错误透传」分支）。
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

// cancelAfterRead 在首次 Read 后取消 ctx（覆盖「拷贝中途取消」分支）。
type cancelAfterRead struct {
	data   []byte
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	r.cancel()
	return n, nil
}

// shortWriter 模拟短写（Write 返回 n < len(p) 且无错误）。
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestCopyWithCtx(t *testing.T) {
	t.Parallel()

	t.Run("完整拷贝并返回字节数", func(t *testing.T) {
		t.Parallel()
		var dst bytes.Buffer
		n, err := CopyWithCtx(context.Background(), &dst, strings.NewReader("hello world"))
		if err != nil || n != 11 {
			t.Fatalf("n=%d err=%v, want 11/nil", n, err)
		}
		if dst.String() != "hello world" {
			t.Fatalf("内容 = %q", dst.String())
		}
	})

	t.Run("已取消的 ctx 立即返回 context.Canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var dst bytes.Buffer
		n, err := CopyWithCtx(ctx, &dst, strings.NewReader("abc"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n != 0 || dst.Len() != 0 {
			t.Fatalf("取消时不应拷贝任何字节：n=%d len=%d", n, dst.Len())
		}
	})

	t.Run("拷贝中途取消：已拷贝字节保留且报 Canceled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var dst bytes.Buffer
		n, err := CopyWithCtx(ctx, &dst, &cancelAfterRead{data: []byte("partial"), cancel: cancel})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n != int64(len("partial")) || dst.String() != "partial" {
			t.Fatalf("应保留已拷贝部分：n=%d content=%q", n, dst.String())
		}
	})

	t.Run("读错误透传", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		var dst bytes.Buffer
		n, err := CopyWithCtx(context.Background(), &dst, &errReader{data: []byte("xy"), err: boom})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if n != 2 {
			t.Fatalf("n=%d, want 2（已读部分应计数）", n)
		}
	})

	t.Run("短写报 ErrShortWrite", func(t *testing.T) {
		t.Parallel()
		n, err := CopyWithCtx(context.Background(), shortWriter{}, strings.NewReader("abc"))
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("err = %v, want io.ErrShortWrite", err)
		}
		if n != 2 {
			t.Fatalf("n=%d, want 2（报告实际写入量）", n)
		}
	})
}
