// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// upload_progress_registry_test.go 锁定二期 T4 的服务端进度注册表契约：
//   - 按路径分别记账（互不串值）；
//   - `done` 取**最大值**（卷层块级并发乱序上报安全）；
//   - 未知 stage 忽略（受控枚举）；
//   - **TTL 淘汰**（过期读不到；注入假时钟，不用 sleep）；
//   - 并发上报安全。

import (
	"context"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestProgressRegistry_PerKeyAndMonotonic 分路径记账 + done 只增不减（乱序安全）。
func TestProgressRegistry_PerKeyAndMonotonic(t *testing.T) {
	t.Parallel()
	r := newUploadProgressRegistry()
	fnA, fnB := r.fnFor("a.bin"), r.fnFor("b.bin")
	fnA(syncpkg.StageEncrypt, 10, 100)
	fnA(syncpkg.StageEncrypt, 4, 100) // 乱序（迟到块）：不得回退
	fnA(syncpkg.StageEncrypt, 30, 100)
	fnB(syncpkg.StageRemote, 7, 50)

	a, ok := r.get("a.bin")
	if !ok {
		t.Fatal("a.bin 应有进度")
	}
	if a.Stage != string(syncpkg.StageEncrypt) || a.EncryptDone != 30 || a.EncryptTotal != 100 {
		t.Fatalf("a.bin 进度不符: %+v", a)
	}
	b, ok := r.get("b.bin")
	if !ok {
		t.Fatal("b.bin 应有进度")
	}
	if b.Stage != string(syncpkg.StageRemote) || b.RemoteDone != 7 || b.RemoteTotal != 50 {
		t.Fatalf("b.bin 进度不符: %+v", b)
	}
	// 不串值：a 的 remote 仍为 0。
	if a.RemoteDone != 0 || a.RemoteTotal != 0 {
		t.Fatalf("不同阶段不得互相污染: %+v", a)
	}
}

// TestProgressRegistry_UnknownStageIgnored 未知 stage 忽略（受控枚举，不得把任意字符串写进快照）。
func TestProgressRegistry_UnknownStageIgnored(t *testing.T) {
	t.Parallel()
	r := newUploadProgressRegistry()
	r.fnFor("x.bin")(syncpkg.UploadStage("bogus"), 5, 10)
	if _, ok := r.get("x.bin"); ok {
		t.Fatal("未知阶段不应产生条目")
	}
}

// TestProgressRegistry_TTL 过期后读不到（假时钟推进，不用 sleep）。
func TestProgressRegistry_TTL(t *testing.T) {
	t.Parallel()
	r := newUploadProgressRegistry()
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	r.fnFor("t.bin")(syncpkg.StageEncrypt, 1, 2)
	if _, ok := r.get("t.bin"); !ok {
		t.Fatal("未过期时应能读到")
	}
	base = base.Add(uploadProgressTTL + time.Second)
	if _, ok := r.get("t.bin"); ok {
		t.Fatal("超过 TTL 应读不到（且被淘汰）")
	}
}

// TestProgressRegistry_Concurrent 并发上报（卷层块级并发）零丢失且最终取最大值。
func TestProgressRegistry_Concurrent(t *testing.T) {
	t.Parallel()
	r := newUploadProgressRegistry()
	fn := r.fnFor("c.bin")
	const goroutines, per = 8, 100
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(base int64) {
			defer wg.Done()
			for i := range per {
				fn(syncpkg.StageEncrypt, base+int64(i+1), 1000)
			}
		}(int64(g * per))
	}
	wg.Wait()
	got, ok := r.get("c.bin")
	if !ok {
		t.Fatal("并发上报后应有条目")
	}
	if want := int64(goroutines * per); got.EncryptDone != want {
		t.Fatalf("done 应取最大值 %d，got %d", want, got.EncryptDone)
	}
	if got.EncryptTotal != 1000 {
		t.Fatalf("total 应为 1000，got %d", got.EncryptTotal)
	}
}

// TestWithUploadProgress_InjectsOnlyWhenAbsent 注入语义：未装配 ⇒ 注入；已装配 ⇒ **不覆盖**。
func TestWithUploadProgress_InjectsOnlyWhenAbsent(t *testing.T) {
	t.Parallel()
	s := &Service{}
	ctx := s.withUploadProgress(context.Background(), "d/a.bin")
	if _, ok := syncpkg.UploadProgressFrom(ctx); !ok {
		t.Fatal("未装配时应注入注册表回调")
	}
	// 已装配 ⇒ 原样返回（不覆盖上层更细的装配）。
	called := 0
	mine := syncpkg.WithUploadProgress(context.Background(),
		func(syncpkg.UploadStage, int64, int64) { called++ })
	got := s.withUploadProgress(mine, "d/a.bin")
	fn, _ := syncpkg.UploadProgressFrom(got)
	fn(syncpkg.StageEncrypt, 1, 2)
	if called != 1 {
		t.Fatal("已装配的回调不得被覆盖")
	}
	// 注入的回调写入注册表 ⇒ 可经 get 读出（端到端串通）。
	fn2, _ := syncpkg.UploadProgressFrom(ctx)
	fn2(syncpkg.StageRemote, 3, 9)
	if e, ok := s.progressRegistry().get("d/a.bin"); !ok || e.RemoteDone != 3 {
		t.Fatalf("注入的回调应把进度写入注册表，got %+v ok=%v", e, ok)
	}
}

// TestFillStageProgress_CandidatesAndCompat 回读契约：候选 key 命中即填；未命中 ⇒ 字段保持零值（向后兼容）。
func TestFillStageProgress_CandidatesAndCompat(t *testing.T) {
	t.Parallel()
	s := &Service{}
	s.progressRegistry().fnFor("d/b.bin")(syncpkg.StageEncrypt, 5, 10)
	resp := &ChunkStatusResponse{Success: true}
	s.fillStageProgress(resp, "d/b.bin")
	if resp.Stage != "encrypt" || resp.EncryptDone != 5 || resp.EncryptTotal != 10 {
		t.Fatalf("命中候选应填充阶段进度，got %+v", resp)
	}
	// 未命中：保持零值（omitempty ⇒ 旧客户端看到的响应与以前完全一致）。
	empty := &ChunkStatusResponse{Success: true}
	s.fillStageProgress(empty, "nope.bin")
	if empty.Stage != "" || empty.EncryptDone != 0 || empty.RemoteDone != 0 {
		t.Fatalf("未命中不得填充任何字段（向后兼容），got %+v", empty)
	}
}
