// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTokenBucket_WaitNBurst 桶满时 WaitN 立即返回（不阻塞），burst 内不限速。
func TestTokenBucket_WaitNBurst(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1000, 4096) // 1KB/s, burst 4KB
	ctx := context.Background()
	start := time.Now()
	if err := b.WaitN(ctx, 1024); err != nil {
		t.Fatalf("WaitN(1024) burst 内应成功: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("burst 内 WaitN 不应阻塞, took %v", d)
	}
}

// TestTokenBucket_WaitNRefills 耗尽后 WaitN 按速率 refill（1KB/s 下 512B 需 ~512ms）。
func TestTokenBucket_WaitNRefills(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1000, 1024) // 1KB/s, burst 1KB（满桶即 1KB）
	ctx := context.Background()
	if err := b.WaitN(ctx, 1024); err != nil { // 消耗满桶
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.WaitN(ctx, 512); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("耗尽后 WaitN(512) 应等待 refill ~512ms, got %v", d)
	}
}

// TestTokenBucket_ContextCancel WaitN 在 ctx 取消时返回错误。
func TestTokenBucket_ContextCancel(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1, 1) // 极慢：1B/s burst 1B
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.WaitN(ctx, 1024); err == nil {
		t.Fatal("ctx 取消后 WaitN 应返回错误")
	}
}

// TestTokenBucket_ZeroRateDisabled rate<=0 表示不限速（默认关零回归）。
func TestTokenBucket_ZeroRateDisabled(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(0, 0)
	start := time.Now()
	if err := b.WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("rate=0 不应限速: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("rate=0 WaitN 不应阻塞, took %v", d)
	}
}

// TestLimiterPerOwner 每个 owner 独立桶：一 owner 限速不影响另一 owner（per-owner 隔离）。
func TestLimiterPerOwner(t *testing.T) {
	t.Parallel()
	lm := NewBandwidthLimiter(func(owner string) *TokenBucket {
		if owner == "slow" {
			return NewTokenBucket(100, 100) // 100B/s burst 100B
		}
		return nil // 其它 owner 不限速
	})
	if lm.BucketFor("slow") == nil {
		t.Fatal("slow owner 应有桶")
	}
	if lm.BucketFor("fast") != nil {
		t.Fatal("fast owner 不应有桶（不限速）")
	}
	// slow 桶消耗后 fast 不受影响。
	slow := lm.BucketFor("slow")
	if err := slow.WaitN(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if err := lm.BucketFor("fast").WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("fast owner WaitN 应不受 slow 影响: %v", err)
	}
}

// TestLimitReader_PreservesRandomReadAndStaysLimited（P2 修复，2026-10-19）：限速包装必须
// **透传** io.Seeker/io.ReaderAt——否则下游流式卷（secretdata）按能力判定（canStream）会把
// 可 seek 源当成纯流 ⇒ 整份明文进内存、本地临时峰值回到 O(文件)，且块级交错的回退链②
// （源可重读 ⇒ 降级内存交付）失效。即：**限速开关会静默关掉本轮的交错上传与回退链**。
// 同时 ReadAt 必须**仍受限速**（随机读若绕开令牌桶 = 限速被架空），Seek 不消耗带宽。
//
// 变异：limitReader 去掉 ReaderAt/Seeker 透传分支 ⇒ 能力断言必红；ReadAt 直通不 WaitN ⇒
// 「第二次 ReadAt 仍在阻塞」断言必红。
func TestLimitReader_PreservesRandomReadAndStaysLimited(t *testing.T) {
	t.Parallel()
	src := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatalf("写源: %v", err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("打开源: %v", err)
	}
	defer f.Close()

	// ① limiter=nil（默认关）⇒ 原样返回，能力与路径零回归。
	if got := limitReader(nil, "o", f); got != io.Reader(f) {
		t.Fatal("limiter=nil 时必须原样返回源（零回归）")
	}
	// ② 该 owner 无桶（不限速）⇒ 原样返回。
	none := NewBandwidthLimiter(func(string) *TokenBucket { return nil })
	if got := limitReader(none, "o", f); got != io.Reader(f) {
		t.Fatal("无该 owner 桶时必须原样返回源（零回归）")
	}
	// ③ 限速生效 ⇒ 透传 Seeker/ReaderAt/SourcePath，且 ReadAt 计入令牌桶。
	bucket := NewTokenBucket(1, 1) // 1B/s、burst 1B：第二次读 1B 必阻塞
	lim := NewBandwidthLimiter(func(string) *TokenBucket { return bucket })
	got := limitReader(lim, "o", f)
	if _, ok := got.(io.Seeker); !ok {
		t.Fatal("限速包装必须透传 io.Seeker（否则流式卷走内存路径）✗")
	}
	ra, ok := got.(io.ReaderAt)
	if !ok {
		t.Fatal("限速包装必须透传 io.ReaderAt（否则 secretdata 块级交错失效）✗")
	}
	sp, ok := got.(interface{ SourcePath() string })
	if !ok || sp.SourcePath() != src {
		t.Fatalf("限速包装必须透传 SourcePath（回退链②判据），实测 %v", sp)
	}
	buf := make([]byte, 1)
	if _, rerr := ra.ReadAt(buf, 0); rerr != nil {
		t.Fatalf("首次 ReadAt 应成功: %v", rerr)
	}
	done := make(chan struct{})
	go func() { _, _ = ra.ReadAt(buf, 1); close(done) }()
	select {
	case <-done:
		t.Fatal("第二次 ReadAt 未受限速（随机读绕开令牌桶）✗")
	case <-time.After(80 * time.Millisecond):
		// 仍在等令牌（1B/s ⇒ ~1s 后自然放行）⇒ ReadAt 确实计入限速 ✓
	}
	// ④ 非 seek 源 ⇒ **不得**假装有随机读能力（假能力会让下游走 ReadAt 并失败）。
	plain := io.NopCloser(bytes.NewReader([]byte("abc"))) // NopCloser 不实现 Seeker/ReaderAt
	if got2 := limitReader(lim, "o", plain); got2 != io.Reader(plain) {
		if _, ok := got2.(io.ReaderAt); ok {
			t.Fatal("非 seek 源不得被包装出 ReaderAt 能力（假能力误导下游）✗")
		}
	}
}
