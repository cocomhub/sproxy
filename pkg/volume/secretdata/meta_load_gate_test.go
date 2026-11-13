// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_load_gate_test.go 钉住 P3 的「meta 加载并发闸」：
// 实测单条加载瞬时峰值可达 ~350 MB（常驻 ~82 MB），并发雪崩会直接 OOM；
// L1b 的字节预算只管「已加载常驻量」，管不了「正在加载的峰值」⇒ 由本闸兜住。

import (
	"context"
	"testing"
	"time"
)

// TestMetaLoadConcurrency_Bounds 锁定上限推导 `min(4, 预算/单条常驻)`，含上下限。
// 变异：去掉封顶（>4）或去掉下限（<1）⇒ 本用例红。
func TestMetaLoadConcurrency_Bounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		budget int64
		want   int
	}{
		{"默认 256 MiB ⇒ 3 条（= 预算/82MB）", int64(defaultMetaCacheBytes), 3},
		{"1 GiB ⇒ 封顶 4（吞吐已饱和）", 1 << 30, 4},
		{"极小预算 ⇒ 下限 1", 1 << 20, 1},
		{"零预算 ⇒ 下限 1", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := metaLoadConcurrency(tc.budget); got != tc.want {
				t.Fatalf("metaLoadConcurrency(%d)=%d, want %d", tc.budget, got, tc.want)
			}
		})
	}
}

// TestMetaLoadGate_BoundsConcurrency 锁定闸真的限制并发：容量满时第 N+1 次 acquire 阻塞，
// release 后才可继续。nil 闸（未装配）恒可获取（零回归）。
func TestMetaLoadGate_BoundsConcurrency(t *testing.T) {
	t.Parallel()
	var nilGate metaLoadGate
	if err := nilGate.acquire(context.Background()); err != nil {
		t.Fatalf("nil 闸应恒可获取: %v", err)
	}
	nilGate.release() // 不得 panic / 阻塞

	g := make(metaLoadGate, 2)
	ctx := context.Background()
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// 第 3 次应阻塞 ⇒ 用带短超时的 ctx 验证「确实拿不到」。
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := g.acquire(tctx); err == nil {
		t.Fatal("容量已满时 acquire 必须阻塞（或按 ctx 超时返回错误）")
	}
	g.release() // 释放一个 ⇒ 现在应能拿到
	if err := g.acquire(ctx); err != nil {
		t.Fatalf("释放后应可获取: %v", err)
	}
}

// TestMetaLoadGate_CtxCancel 锁定额满时 ctx 取消立即返回（不无限等待）。
func TestMetaLoadGate_CtxCancel(t *testing.T) {
	t.Parallel()
	g := make(metaLoadGate, 1)
	if err := g.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- g.acquire(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("已取消的 ctx 应在额满时返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 已取消却仍在等待（应立即可返回）")
	}
}
