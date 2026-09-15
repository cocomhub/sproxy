// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件钉住 `Stream.Abort()` 的**注销语义**。
//
// 动机（2026-09-16 独立审计的已确认缺陷）：`Abort` 只做 `closeChannels()`——不从 `m.streams`
// 移除、也不递减 `activeStreams`。计数递减只存在于 `removeStream`（`mux.go`，本地 Close 路径
// 与 `handleCloseFrame`/`handleRejectFrame`）。`Abort` 是**非阻塞强制释放**路径（writeCh 打满时
// `Close` 会永久阻塞，见 stream.Abort 文档与 pkg/iostream），因此一旦走 Abort，流表项与并发计数
// **永久残留**；配合 `WithMaxStreams(n)` 的门禁就会**永久拒绝新流**（与 pkg/cloud 修过的
// 「置标记但没有清理者」同型）。
//
// 修正理由口径：生产路径当前**不设** maxStreams（`WithMaxStreams` 只被测试使用），因此今天
// 的主要危害是「每条被 Abort 的流永久留在 `m.streams` + `activeStreams` 虚高」——在长寿 mux
// （hub↔leaf 隧道、`pkg/server/relay_stream.go`、`mesh/gateway.go`、`pkg/sync/httptransport`
// 的 deadline 强制关闭）上按被放弃的流数**无界累积**；maxStreams 场景是同一缺陷的放大版。
//
// 修复口径：Abort 与 Close/对端 Close 共用同一套注销逻辑（`Mux.removeStreamIf`，按**对象身份**
// 而非只按 sid——旧句柄在「对端复用同 sid」时不得误伤别人的活流），幂等闸门是「表里登记的是不是
// 这个对象」，保证重复 Abort / Abort+Close / Abort+对端 Close 交叉都只递减一次。

// TestStreamAbort_UnregistersStream：Abort 之后流表与 activeStreams 都必须归零，
// 且 maxStreams 额度被释放（能再开新流）。
func TestStreamAbort_UnregistersStream(t *testing.T) {
	t.Parallel()
	a, _ := xfertest.Pipe()
	m := NewWithOpts(a, RoleDialer, WithMaxStreams(1))
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()

	s1, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("第一条流应能打开: %v", err)
	}
	if got := m.activeStreams.Load(); got != 1 {
		t.Fatalf("Open 后 activeStreams=%d want 1", got)
	}
	if _, err = m.Open(ctx); !errors.Is(err, ErrMaxStreams) {
		t.Fatalf("maxStreams=1 时第二条流应被拒（ErrMaxStreams）, got %v", err)
	}

	if err = s1.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	m.mu.Lock()
	_, stillRegistered := m.streams[s1.ID()]
	m.mu.Unlock()
	if stillRegistered {
		t.Error("Abort 后流表不得残留该流（否则表项与计数永久泄漏）")
	}
	if got := m.activeStreams.Load(); got != 0 {
		t.Errorf("Abort 后 activeStreams=%d want 0", got)
	}

	// Abort 的既有契约（必须保留）：done 关闭以解除 Read/Write 阻塞。
	select {
	case <-s1.(*stream).done:
	default:
		t.Error("Abort 必须关闭 done（否则被阻塞的 Read/Write 永不返回）")
	}

	// 关键行为：额度被释放 ⇒ 能再开新流（修复前这里会因 activeStreams 残留而永久 ErrMaxStreams）。
	s2, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Abort 释放额度后应能再开新流（修复前为 ErrMaxStreams）: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if got := m.activeStreams.Load(); got != 1 {
		t.Errorf("重开新流后 activeStreams=%d want 1", got)
	}
}

// TestStreamAbort_IdempotentWithClose：重复 Abort、Abort 与 Close 交叉调用都只能递减一次
// （不得把 activeStreams 打成负数）。
func TestStreamAbort_IdempotentWithClose(t *testing.T) {
	t.Parallel()
	a, _ := xfertest.Pipe()
	m := NewWithOpts(a, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	_ = s.Abort()
	_ = s.Abort()
	_ = s.Close() // Close 经 writeCh：done 已关时立即返回错误，不阻塞
	_ = s.Abort()

	if got := m.activeStreams.Load(); got != 0 {
		t.Errorf("重复注销后 activeStreams=%d want 0（不得重复递减）", got)
	}
	m.mu.Lock()
	size := len(m.streams)
	m.mu.Unlock()
	if size != 0 {
		t.Errorf("重复注销后流表应为空, got %d 项", size)
	}
}

// TestStreamAbort_IdempotentWithPeerClose：Abort 与**对端 Close 帧**交叉（两种顺序）都只递减一次。
//
// 为什么单独一条：既有 `TestStreamAbort_IdempotentWithClose` 只覆盖本地 `Close`（走 writeLoop 的
// 注销），而 `stream.Abort` 的文档明确声称「Abort 与对端 Close 交叉也只递减一次」——对端
// `FrameClose` 的递减点在 `handleCloseFrame → Mux.removeStream`，此前**没有**任何用例把它与
// Abort 交叉起来。若实现对端 Close 时无条件递减，第二种顺序会把计数打成 -1，本用例即红。
func TestStreamAbort_IdempotentWithPeerClose(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	run := func(name string, peerCloseFirst bool) {
		t.Run(name, func(t *testing.T) {
			a, b := xfertest.Pipe()
			m := NewWithOpts(a, RoleDialer)
			t.Cleanup(func() { _ = m.Close() })

			s, err := m.Open(ctx)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			sid := s.ID()

			peerClose := func() {
				raw, encErr := EncodeFrame(sid, FrameClose, nil)
				if encErr != nil {
					t.Fatalf("编码 Close 帧: %v", encErr)
				}
				before := m.Metrics().Streams.Closed.Load()
				if sErr := b.Send(ctx, raw); sErr != nil {
					t.Fatalf("投递 Close 帧: %v", sErr)
				}
				// 有界等待：`handleCloseFrame` 处理完该帧后会递增 Streams.Closed（无条件递增，
				// 因此无论流表里是否还有该 sid 都能作为「已处理」的证据）。
				testutil.WaitFor(t, 10*time.Second, func() bool {
					return m.Metrics().Streams.Closed.Load() > before
				}, "对端 Close 帧应被读循环处理完（Streams.Closed 计数前进）")
			}

			if peerCloseFirst {
				peerClose()
			}
			if err = s.Abort(); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			if !peerCloseFirst {
				peerClose()
			}

			if got := m.activeStreams.Load(); got != 0 {
				t.Errorf("Abort 与对端 Close 交叉后 activeStreams=%d want 0（不得双递减/不得为负）", got)
			}
			m.mu.Lock()
			size := len(m.streams)
			m.mu.Unlock()
			if size != 0 {
				t.Errorf("交叉注销后流表应为空, got %d 项", size)
			}
		})
	}

	run("对端 Close 先、Abort 后", true)
	run("Abort 先、对端 Close 后", false)
}

// TestStreamAbort_StaleHandleDoesNotKillReusedID：注销闸门必须是**对象身份**而非只按 streamID。
//
// 可达性：`handleOpenFrame` 对**对端发来的任意 sid** 只做 exists 检查（不校验角色/单调性），
// 于是「对端先 Close/Reject 摘掉该 sid → 再用同一 sid 开新流」之后，本地**旧句柄**若按 id 注销，
// 就会摘掉并 `closeChannels()` 掉**别人的活流**、并把 `activeStreams` 错误递减。参考实现对端 id
// 单调 +2 不复用，故只有不守协议/恶意对端能触发；但调用方确实存在「持已失效句柄、稍后才 Abort」
// 的形态（`pkg/server/relay_stream.go` 的失败/超时/空闲路径）。
//
// 本用例用**公开路径**（旧句柄的 `Abort()`）钉住该语义：修复前（按 id 注销）三条断言全红。
func TestStreamAbort_StaleHandleDoesNotKillReusedID(t *testing.T) {
	t.Parallel()
	a, _ := xfertest.Pipe()
	m := NewWithOpts(a, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	live, err := m.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	liveStream := live.(*stream)
	sid := liveStream.ID()

	// 同 id 的**另一个对象**：模拟「对端复用 sid 后本地仍持有的旧句柄」。
	stale := newStream(sid, m)
	if stale == liveStream {
		t.Fatal("测试自检失败：两个句柄必须是不同对象")
	}

	if err = stale.Abort(); err != nil {
		t.Fatalf("旧句柄 Abort: %v", err)
	}

	m.mu.Lock()
	cur, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok || cur != liveStream {
		t.Error("旧句柄不得摘除/替换表内当前登记的流（同 sid 已被对端复用）")
	}
	if got := m.activeStreams.Load(); got != 1 {
		t.Errorf("旧句柄不得递减计数：activeStreams=%d want 1", got)
	}
	select {
	case <-liveStream.done:
		t.Error("旧句柄不得关闭表内活流的 done")
	default:
	}

	// 契约保留：Abort 必须关闭「被调用句柄」自己的 done（与被注销对象无关）。
	select {
	case <-stale.done:
	default:
		t.Error("Abort 必须关闭**被调用句柄**的 done（否则调用方的 Read/Write 永不返回）")
	}

	// 正确句柄仍能正常注销，且计数只减到 0。
	if err = liveStream.Abort(); err != nil {
		t.Fatalf("活流 Abort: %v", err)
	}
	if got := m.activeStreams.Load(); got != 0 {
		t.Errorf("活流 Abort 后 activeStreams=%d want 0", got)
	}
}

// TestStreamAbort_UnblocksRead：Abort 必须让阻塞中的 Read 立即返回（既有契约，防修复回归）。
func TestStreamAbort_UnblocksRead(t *testing.T) {
	t.Parallel()
	a, _ := xfertest.Pipe()
	m := NewWithOpts(a, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	s, err := m.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, rerr := s.Read(buf)
		errCh <- rerr
	}()

	// 等 Read 真正进入阻塞：done 未关且无数据时它必然在等 dataCh/done。
	// （不引入固定 sleep：直接 Abort，随后用有界等待收敛。）
	if err := s.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case rerr := <-errCh:
		if rerr == nil {
			t.Error("Abort 后阻塞的 Read 应返回错误，got nil")
		}
	case <-ctx.Done():
		t.Fatal("Abort 后阻塞的 Read 未返回（既有契约被破坏）")
	}
}
