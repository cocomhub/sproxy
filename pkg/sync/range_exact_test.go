// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"io"
	"strings"
	"testing"
)

// range_exact_test.go 钉住 **RangeReader 长度契约**（2026-10-14 跨层扫雷）：
//   - size==0 ⇒ 空区间成功（本地卷/加密卷/远端/baidupcs 必须同语义）；
//   - 越出文件 ⇒ 错误（fail-closed，不得静默短读）；
//   - 提前 EOF ⇒ RangeExactReader 报错（服务端钳制/对象短于声明的兜底）。
//
// 变异：去掉 RangeExactReader 的提前 EOF 判据 ⇒ 短读用例红；去掉本地 size==0 早返回 ⇒ 契约用例红。

// TestRangeExactReader_ExactFill：恰好读满 ⇒ 无错且读满后 EOF。
func TestRangeExactReader_ExactFill(t *testing.T) {
	t.Parallel()
	rc := ExactRangeReader(io.NopCloser(strings.NewReader("abcdefghij")), 4)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读满不应报错: %v", err)
	}
	if string(got) != "abcd" {
		t.Fatalf("got %q want %q", got, "abcd")
	}
}

// TestRangeExactReader_ShortReadIsError：底层提前结束 ⇒ **错误**（不是安静的少给字节）。
func TestRangeExactReader_ShortReadIsError(t *testing.T) {
	t.Parallel()
	rc := ExactRangeReader(io.NopCloser(strings.NewReader("abc")), 5)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err == nil {
		t.Fatalf("提前 EOF 必须报错（静默短读 = 截断型缺陷），got %q", got)
	}
	if !strings.Contains(err.Error(), "提前结束") {
		t.Fatalf("错误应可归因「区间流提前结束」: %v", err)
	}
}

// TestRangeExactReader_ZeroSizePassthrough：size<=0 不做限制（空区间由调用方直接返回空流）。
func TestRangeExactReader_ZeroSizePassthrough(t *testing.T) {
	t.Parallel()
	inner := io.NopCloser(strings.NewReader("xy"))
	if got := ExactRangeReader(inner, 0); got != inner {
		t.Fatal("size==0 应原样返回（不做长度限制）")
	}
}

// TestLocalFS_OpenRangeRead_LengthContract：本地卷作为契约基准——空区间成功、越界/非法报错。
func TestLocalFS_OpenRangeRead_LengthContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewLocalFS(dir, nil)
	if err := fs.WriteFile(context.Background(), LogicalKey{}, "f.bin", strings.NewReader("0123456789"), 10, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx := context.Background()

	// size==0（含 offset == 文件尾）⇒ 空流 + 无错误。
	for _, off := range []int64{0, 4, 10} {
		rc, err := fs.OpenRangeRead(ctx, "f.bin", off, 0)
		if err != nil {
			t.Fatalf("size==0（offset=%d）应成功返回空流: %v", off, err)
		}
		got, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil || len(got) != 0 {
			t.Fatalf("size==0（offset=%d）应为空流, got %q err=%v", off, got, rerr)
		}
	}
	// 越出文件 ⇒ 错误（fail-closed，与 baidupcs 服务端钳制场景对照）。
	if _, err := fs.OpenRangeRead(ctx, "f.bin", 8, 5); err == nil {
		t.Fatal("越出区间必须报错（fail-closed）")
	}
	// 非法区间 ⇒ 错误。
	if _, err := fs.OpenRangeRead(ctx, "f.bin", -1, 1); err == nil {
		t.Fatal("负 offset 必须报错")
	}
	if _, err := fs.OpenRangeRead(ctx, "f.bin", 0, -1); err == nil {
		t.Fatal("负 size 必须报错")
	}
	// 越出文件但落在边界内 ⇒ 正常（衔接上面 size==0 分支的边界）。
	rc, err := fs.OpenRangeRead(ctx, "f.bin", 8, 2)
	if err != nil {
		t.Fatalf("边界内区间应成功: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "89" {
		t.Fatalf("边界区间 got %q want %q", got, "89")
	}
}

// TestEmptyRangeReader_EmptyStream：空区间流读即 EOF（各实现统一复用）。
func TestEmptyRangeReader_EmptyStream(t *testing.T) {
	t.Parallel()
	rc := EmptyRangeReader()
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || len(got) != 0 {
		t.Fatalf("空区间流应立即可读尽且无错, got %q err=%v", got, err)
	}
}
