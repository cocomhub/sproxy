// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_pool_wait_test.go 锁定评审 R14 C4：卷容量池在**被在途预留占满**时必须排队等待，
// 而不是一律立即 507；而「已提交数据占满」（没有任何释放可能）仍应**立即失败**（不空等）。

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// poolWaitSet 构造只含一个卷 + 指定容量池的卷集（reserveVolumePool 只读 Pool）。
func poolWaitSet(capacity int64) *registry.Set {
	return registry.NewSet(
		[]volume.Volume{{Name: "v1"}},
		nil, nil,
		map[string]*quota.Pool{"v1": quota.NewPool(capacity)},
		"v1",
	)
}

// TestReserveVolumePool_WaitsForInFlightReservation 在途预留释放后必须成功（不再直接 507）。
func TestReserveVolumePool_WaitsForInFlightReservation(t *testing.T) {
	t.Parallel()
	vs := poolWaitSet(10)
	h := &Handlers{volSet: vs, quotaWaitTimeout: 3 * time.Second}

	held, err := vs.Pool("v1").TryReserve(10)
	if err != nil {
		t.Fatalf("占满卷池失败: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		route := &volumeRoute{volumeName: "v1"}
		done <- h.reserveVolumePool(context.Background(), route, "v1", nil, "user/a.bin", 5)
	}()

	// 确认等待者已入队（避免「先释放后入队」走快路导致假绿）。
	waitSpin(t, 20000, func() bool { return vs.Pool("v1").WaitStats().Waiting > 0 })

	held.Release() // 在途写结束 ⇒ 释放容量

	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("在途预留释放后应成功预留（C4：不得一律 507），got %v", werr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待未返回（可能未排队）")
	}
	if got := vs.Pool("v1").Reserved(); got != 5 {
		t.Fatalf("成功后卷池 reserved=%d, want 5（预留必须持有到 route.release）", got)
	}
}

// TestReserveVolumePool_CommittedFullFailsFast 已提交占满 ⇒ 立即 507（不空等）。
func TestReserveVolumePool_CommittedFullFailsFast(t *testing.T) {
	t.Parallel()
	vs := poolWaitSet(5)
	h := &Handlers{volSet: vs, quotaWaitTimeout: 5 * time.Second} // 若误排队会明显变慢/超时

	// 已提交占用 5B（不是预留）⇒ 没有释放可能（Adjust(prev,next) 直接把 committed 校准为 next）。
	vs.Pool("v1").Adjust(0, 5)

	start := time.Now()
	route := &volumeRoute{volumeName: "v1"}
	err := h.reserveVolumePool(context.Background(), route, "v1", nil, "user/b.bin", 5)
	if err == nil {
		t.Fatal("已提交占满时不得成功")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("已提交占满应立即失败，实际耗时 %v（不应排队空等）", elapsed)
	}
}

// waitSpin 自旋等待条件成立（runtime.Gosched 让出，避免 time.Sleep 棘轮）。
func waitSpin(t *testing.T, maxIter int, cond func() bool) {
	t.Helper()
	for range maxIter {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("自旋等待超时（等待者未入队，用例无效）")
}
