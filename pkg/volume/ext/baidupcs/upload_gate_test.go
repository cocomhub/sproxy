// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// upload_gate_test.go 锁定 **G2：每卷上传并发闸**（复用 `pkg/waitq`，不新造排队原语）。
//
// 为什么需要：并发上传无闸门时并发数会**叠乘** —— secretdata 块级并发（默认 4）× 内核分片并发
// （默认 4）= 单卷 16 并发请求，多卷再叠 ⇒ 触发百度风控（迁移期实测过 31034）。闸门把
// 「同时进行的**上传动作**」限制在每卷上限内（`StorageConfig.UploadConcurrency`，默认 4）。
//
// 同时锁定 **G8**：`PutDeferred` 必须与 `Put` 走**同一上传入口**（`uploadStaged`）——
// 否则它既绕过闸门、也绕过上传内核（无续传/无收益）✗。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedAdapter 记录并发峰值，并可在 `Upload` 内阻塞以制造**确定性**的重叠窗口（不用 sleep）。
type gatedAdapter struct {
	*fakeStorageAdapter
	mu        sync.Mutex
	cur, peak int
	gate      chan struct{} // 非 nil：Upload 内等待（close 放行）
	entered   chan struct{} // 每次进入 Upload 发信号（带缓冲，避免阻塞被测代码）
}

func newGatedAdapter() *gatedAdapter {
	return &gatedAdapter{fakeStorageAdapter: newFakeStorageAdapter()}
}

func (g *gatedAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	g.mu.Lock()
	g.cur++
	if g.cur > g.peak {
		g.peak = g.cur
	}
	g.mu.Unlock()
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
			g.mu.Lock()
			g.cur--
			g.mu.Unlock()
			return ctx.Err()
		}
	}
	err := g.fakeStorageAdapter.Upload(ctx, localPath, targetPath, overwrite)
	g.mu.Lock()
	g.cur--
	g.mu.Unlock()
	return err
}

func (g *gatedAdapter) peakN() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// newGateStorage 建一个走 fake adapter（⇒ 上传落到 legacy 适配器路径）的 Storage。
func newGateStorage(t *testing.T, ad Adapter, limit int) *Storage {
	t.Helper()
	st, err := NewStorage(StorageConfig{
		Root: "/", TempDir: t.TempDir(), Adapter: ad, Logger: testLogger(),
		UploadConcurrency: limit,
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return st
}

// TestStorage_UploadGate_Normalization 归一化：0/负数 ⇒ 默认；超上限 ⇒ 钳到上限（硬规则 7c：默认值即安全路径）。
func TestStorage_UploadGate_Normalization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want int
	}{
		{0, DefaultUploadConcurrency},
		{-3, DefaultUploadConcurrency},
		{1, 1},
		{4, 4},
		{100, MaxUploadConcurrency},
	} {
		st := newGateStorage(t, newGatedAdapter(), tc.in)
		if got := st.uploadLimit; got != tc.want {
			t.Errorf("UploadConcurrency=%d ⇒ 上限 %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestStorage_UploadGate_LimitsConcurrency 闸门必须真的限制同时进行的上传数。
func TestStorage_UploadGate_LimitsConcurrency(t *testing.T) {
	t.Parallel()
	const limit = 3
	ad := newGatedAdapter()
	ad.gate = make(chan struct{})
	ad.entered = make(chan struct{}, 16)
	st := newGateStorage(t, ad, limit)

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("dir/f%d.bin", i)
			if _, err := st.Put(context.Background(), key, strings.NewReader("x")); err != nil {
				t.Errorf("Put %s: %v", key, err)
			}
		}(i)
	}
	// 确定性同步：恰好 limit 个进入 Upload（第 limit+1 个必然被闸门挡住）。
	for range limit {
		select {
		case <-ad.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("等待上传进入超时（闸门可能未生效）")
		}
	}
	if got := ad.peakN(); got != limit {
		t.Fatalf("同时进行的上传数应为闸门上限 %d，got %d", limit, got)
	}
	// **负向断言（防假绿）**：闸门已满 ⇒ 第 4 个上传必须仍在闸门外等待，
	// 不得进入 `Upload`。若只断言上面的 peak，移除闸门后（第 4 个尚未被调度）仍会通过 ✗。
	select {
	case <-ad.entered:
		t.Fatalf("闸门上限 %d 已满，第 %d 个上传不得进入 Upload（闸门未生效）", limit, limit+1)
	case <-time.After(300 * time.Millisecond):
		// 正确：被闸门挡住
	}
	close(ad.gate)
	wg.Wait()
	if got := ad.peakN(); got > limit {
		t.Fatalf("并发峰值 %d 超过闸门上限 %d", got, limit)
	}
}

// TestStorage_UploadGate_WaitCanceled 闸门等待中 ctx 取消 ⇒ 立即返回错误（不得永久阻塞）。
func TestStorage_UploadGate_WaitCanceled(t *testing.T) {
	t.Parallel()
	ad := newGatedAdapter()
	ad.gate = make(chan struct{})
	ad.entered = make(chan struct{}, 4)
	st := newGateStorage(t, ad, 1)

	first := make(chan error, 1)
	go func() {
		_, err := st.Put(context.Background(), "dir/a.bin", strings.NewReader("a"))
		first <- err
	}()
	<-ad.entered // 占住唯一槽位

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消 ⇒ 等待即刻失败
	if _, err := st.Put(ctx, "dir/b.bin", strings.NewReader("b")); err == nil {
		t.Fatal("闸门等待中 ctx 已取消必须返回错误（不得永久阻塞）")
	}
	close(ad.gate)
	if err := <-first; err != nil {
		t.Fatalf("占用槽位的首个上传应成功: %v", err)
	}
}

// TestStorage_PutDeferred_GoesThroughUploadGate（G8）`PutDeferred` 必须与 `Put` 同一上传入口：
// 闸门被占满时它也必须排队（若不排队 ⇒ 绕过闸门，也绕过内核）。
func TestStorage_PutDeferred_GoesThroughUploadGate(t *testing.T) {
	t.Parallel()
	ad := newGatedAdapter()
	ad.gate = make(chan struct{})
	ad.entered = make(chan struct{}, 4)
	st := newGateStorage(t, ad, 1)

	local := filepath.Join(t.TempDir(), "deferred.bin")
	if err := os.WriteFile(local, []byte("deferred-content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	first := make(chan error, 1)
	go func() {
		_, err := st.Put(context.Background(), "dir/a.bin", strings.NewReader("a"))
		first <- err
	}()
	<-ad.entered // 占住唯一槽位

	done := make(chan error, 1)
	go func() {
		done <- st.PutDeferred(context.Background(), "dir/deferred.bin", local, int64(len("deferred-content")), nil)
	}()
	select {
	case err := <-done:
		t.Fatalf("PutDeferred 必须与 Put 共用闸门 ⇒ 应被阻塞，但它提前返回: %v", err)
	case <-time.After(300 * time.Millisecond):
		// 正确：被闸门挡住
	}
	close(ad.gate)
	if err := <-done; err != nil {
		t.Fatalf("放行后 PutDeferred 应成功: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("首个上传应成功: %v", err)
	}
	if got := ad.peakN(); got > 1 {
		t.Fatalf("闸门=1 时并发峰值应 ≤1，got %d", got)
	}
}
