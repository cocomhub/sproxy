// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// countingFallback 记录库兜底 Upload 是否被调用。
type countingFallback struct {
	uploads int
}

func (c *countingFallback) Upload(context.Context, string, string, bool) error {
	c.uploads++
	return nil
}
func (c *countingFallback) Download(context.Context, string, string) error { return ErrUnsupported }
func (c *countingFallback) Move(context.Context, string, string) error     { return ErrUnsupported }
func (c *countingFallback) Copy(context.Context, string, string) error     { return ErrUnsupported }
func (c *countingFallback) Delete(context.Context, string) error           { return ErrUnsupported }

var _ libraryFallback = (*countingFallback)(nil)

// TestBinaryAdapter_Upload_NoSilentLibraryFallback 钉住 **L1**（2026-11-08 实测）：
// 库上传实测**小文件 ~90s 且不响应 ctx**（P7：`phase=upload elapsed_ms=89710`）⇒
// **上传默认禁止库兜底**：二进制失败 ⇒ **显式报错**（fail-closed），**不得**静默落到库路径。
// **变异**：恢复"无条件回退库" ⇒ 本用例红（错误为 nil 且 fallback.uploads==1）。
func TestBinaryAdapter_Upload_NoSilentLibraryFallback(t *testing.T) {
	t.Parallel()
	fb := &countingFallback{}
	// BinaryPath 指向不存在路径 ⇒ runBinary 必失败 ⇒ 触发兜底决策。
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath: "/nonexistent/BaiduPCS-Go-missing",
		Fallback:   fb,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	err := a.Upload(context.Background(), "/tmp/x", "dir/x", true)
	if err == nil {
		t.Fatal("默认策略下二进制失败必须显式报错（不得静默降级到库上传）")
	}
	if fb.uploads != 0 {
		t.Fatalf("默认策略下不得调用库上传兜底，got %d 次", fb.uploads)
	}
	if !strings.Contains(err.Error(), "库兜底未启用") {
		t.Fatalf("错误信息应指明「库兜底未启用」以便运维定位，got %v", err)
	}
}

// TestBinaryAdapter_Upload_LibraryFallbackWhenExplicitlyAllowed 显式开启时才允许库兜底。
func TestBinaryAdapter_Upload_LibraryFallbackWhenExplicitlyAllowed(t *testing.T) {
	t.Parallel()
	fb := &countingFallback{}
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath:         "/nonexistent/BaiduPCS-Go-missing",
		Fallback:           fb,
		AllowLibraryUpload: true,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err := a.Upload(context.Background(), "/tmp/x", "dir/x", true); err != nil {
		t.Fatalf("显式允许时应走库兜底并不报错，got %v", err)
	}
	if fb.uploads != 1 {
		t.Fatalf("应调用库兜底一次，got %d", fb.uploads)
	}
}
