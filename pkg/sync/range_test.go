// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalFS_OpenRangeRead 验证 LocalFS 实现 RangeReader：按 [offset,size) 局部读取
// 底层文件，返回该区间字节。
func TestLocalFS_OpenRangeRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := make([]byte, 1000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "f.bin"), content, 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	l := NewLocalFS(root, nil)
	// 类型断言：LocalFS 应实现 RangeReader。
	rr, ok := any(l).(RangeReader)
	if !ok {
		t.Fatal("LocalFS 应实现 RangeReader")
	}
	ctx := context.Background()
	rc, err := rr.OpenRangeRead(ctx, "f.bin", 100, 200)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := content[100:300]
	if len(got) != len(want) {
		t.Fatalf("读区间长度=%d，应为 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("区间[%d]字节=%d，应为 %d（局部读偏移错误）", i, got[i], want[i])
		}
	}
}

// TestLocalFS_OpenRangeRead_SubRange 验证任意子区间读取（非对齐、超文件尾拒绝）。
func TestLocalFS_OpenRangeRead_SubRange(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	content := bytes.Repeat([]byte{0xAB}, 500)
	if err := os.WriteFile(filepath.Join(root, "f.bin"), content, 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	l := NewLocalFS(root, nil)
	ctx := context.Background()
	rc, err := l.OpenRangeRead(ctx, "f.bin", 250, 100)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("子区间长度=%d，应为 100", len(got))
	}
	for _, b := range got {
		if b != 0xAB {
			t.Fatalf("子区间内容错误: %02x", b)
		}
	}
}

// TestLocalFS_OpenRangeRead_Bounds：offset<0 / size<0 / 越出文件 → 报错（fail-closed）。
func TestLocalFS_OpenRangeRead_Bounds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.bin"), bytes.Repeat([]byte{1}, 100), 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	l := NewLocalFS(root, nil)
	ctx := context.Background()
	if _, err := l.OpenRangeRead(ctx, "f.bin", -1, 10); err == nil {
		t.Error("offset<0 应报错")
	}
	if _, err := l.OpenRangeRead(ctx, "f.bin", 0, -1); err == nil {
		t.Error("size<0 应报错")
	}
	if rc, err := l.OpenRangeRead(ctx, "f.bin", 95, 10); err == nil {
		rc.Close()
		t.Error("越出文件尾应报错")
	}
}
