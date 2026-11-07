// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"testing"
)

// fakeBatchVerifier 只实现 FS 基础面 + 批量校验能力（用嵌入接口省掉无关方法）。
type fakeBatchVerifier struct {
	FS
	calls int
}

func (f *fakeBatchVerifier) VerifyMany(_ context.Context, items []VerifyItem) []VerifyResult {
	f.calls++
	out := make([]VerifyResult, len(items))
	for i, it := range items {
		out[i] = VerifyResult{Item: it, State: VerifyOK}
	}
	return out
}

func (f *fakeBatchVerifier) RapidRefresh(_ context.Context, _ VerifyItem) (bool, error) {
	return true, nil
}

// fakePlainFS 只有基础面（无批量校验/秒传刷新能力）。
type fakePlainFS struct{ FS }

// TestPrefixFSForwardsBatchCapabilities 钉住**透明装饰器必须转发 pkg/sync 可选能力**（门禁 R21
// 的运行时对侧）：底层支持 ⇒ 透传（且断言为真）；底层不支持 ⇒ 断言仍为真（装饰器无条件实现），
// 但 VerifyMany 返回 **nil**（长度不符 ⇒ 调用方走「无法判定 ⇒ 跳过批量校验」防御分支），
// RapidRefresh 返回 ErrUnsupported（调用方回落重传）。**不得**返回「全部 mismatch」。
func TestPrefixFSForwardsBatchCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	inner := &fakeBatchVerifier{}
	pf := NewPrefixFS(inner, "pre/")
	bv, ok := AssertBatchVerifier(pf)
	if !ok {
		t.Fatal("装饰器应无条件实现 BatchVerifier（透明转发）")
	}
	got := bv.VerifyMany(ctx, []VerifyItem{{Key: "pre/a", MD5: "x"}, {Key: "pre/b"}})
	if len(got) != 2 || got[0].State != VerifyOK {
		t.Fatalf("应透传底层结果，got %+v", got)
	}
	if inner.calls != 1 {
		t.Fatalf("底层应被调用一次，got %d", inner.calls)
	}
	rr, ok := AssertRapidRefresher(pf)
	if !ok {
		t.Fatal("装饰器应无条件实现 RapidRefresher（透明转发）")
	}
	if hit, err := rr.RapidRefresh(ctx, VerifyItem{Key: "pre/a"}); err != nil || !hit {
		t.Fatalf("应透传秒传刷新结果，got %v/%v", hit, err)
	}

	// 底层不支持：断言仍真（装饰器实现），但必须表达「无法判定/不支持」而不是假失败。
	plain := NewPrefixFS(&fakePlainFS{}, "pre/")
	bv2, ok := AssertBatchVerifier(plain)
	if !ok {
		t.Fatal("装饰器应无条件实现 BatchVerifier")
	}
	if res := bv2.VerifyMany(ctx, []VerifyItem{{Key: "pre/a"}}); res != nil {
		t.Fatalf("底层不支持时应返回 nil（长度不符 ⇒ 调用方跳过），got %+v", res)
	}
	rr2, _ := AssertRapidRefresher(plain)
	if _, err := rr2.RapidRefresh(ctx, VerifyItem{Key: "pre/a"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("底层不支持时应返回 ErrUnsupported，got %v", err)
	}
}
