// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"testing"
)

// TestUploadHints_RoundTrip：ctx 通道往返（nil 不传递）。
func TestUploadHints_RoundTrip(t *testing.T) {
	t.Parallel()
	if _, ok := UploadHintsFrom(context.Background()); ok {
		t.Fatal("无提示时 ok 应为 false")
	}
	in := &UploadHints{Size: 42, MD5: "abc", Path: "/x", RandomContent: true, MTime: 7}
	got, ok := UploadHintsFrom(WithUploadHints(context.Background(), in))
	if !ok || got != in {
		t.Fatalf("应取回同一指针，got %+v ok=%v", got, ok)
	}
	if _, ok := UploadHintsFrom(WithUploadHints(context.Background(), nil)); ok {
		t.Fatal("nil 提示不应放入 ctx")
	}
}

// TestUploadHints_Merge_OnlyFillsGaps：Merge 只补空字段，不覆盖调用方已提供的值
// （调用方更接近源头，优先级更高）——「外层未知 ⇒ 内层首次计算补齐」的核心语义。
func TestUploadHints_Merge_OnlyFillsGaps(t *testing.T) {
	t.Parallel()
	caller := &UploadHints{Size: 100, MD5: "caller-md5", RandomContent: true}
	computed := &UploadHints{Size: 999, MD5: "computed-md5", SHA256: "computed-sha", Path: "/f", MTime: 5}
	got := caller.Merge(computed)
	if got.Size != 100 || got.MD5 != "caller-md5" {
		t.Fatalf("调用方已提供字段不得被覆盖：size=%d md5=%s", got.Size, got.MD5)
	}
	if got.SHA256 != "computed-sha" || got.Path != "/f" || got.MTime != 5 {
		t.Fatalf("缺失字段应由 computed 补齐：%+v", got)
	}
	if !got.RandomContent {
		t.Fatal("RandomContent 是调用方语义标记，应保留")
	}
	if caller.SHA256 != "" {
		t.Fatal("Merge 不得改写原值（只返回副本）")
	}
}

// TestUploadHints_Merge_NilSafe：nil 接收者/nil 参数安全（后端「无提示」时必须仍可用）。
func TestUploadHints_Merge_NilSafe(t *testing.T) {
	t.Parallel()
	var nilHints *UploadHints
	if got := nilHints.Merge(nil); got == nil || got.Size != 0 {
		t.Fatalf("nil.Merge(nil) 应返回空副本，got %+v", got)
	}
	got := nilHints.Merge(&UploadHints{Size: 7, MD5: "m"})
	if got.Size != 7 || got.MD5 != "m" {
		t.Fatalf("nil 接收者应完全采用 computed：%+v", got)
	}
	// 全空调用方 + 全空 computed（后端首次计算失败/无能力）→ 空副本，不 panic。
	if got := (&UploadHints{}).Merge(nil); got == nil {
		t.Fatal("空提示 Merge(nil) 应返回空副本")
	}
}
