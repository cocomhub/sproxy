// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMaybeCleanupClientTemp_Throttled 锁定 2026-11-10 实测缺陷的修复：
// 孤儿清理此前用 `sync.Once` ⇒ **每个进程只清一次** ⇒ 批量上传这类长跑进程运行期间产生的
// 孤儿（请求中断/进程被强杀）**永远清不到**，`%TEMP%/sproxy-client/` 累积到 64GiB ✗。
// 修复为**按时间节流**：首次立即清，之后每 clientTempCleanupInterval 清一次。
func TestMaybeCleanupClientTemp_Throttled(t *testing.T) {
	// sproxy:serial: 需读写包内节流状态（clientTempCleanupAt），本质串行；非全局 seam 替换。
	// 不并行：操作包内清理时间戳（同包测试，按纪律不依赖全局 seam 替换，
	// 这里只是重置自己的节流状态）。
	reset := func(at time.Time) {
		clientTempCleanupMu.Lock()
		clientTempCleanupAt = at
		clientTempCleanupMu.Unlock()
	}
	readAt := func() time.Time {
		clientTempCleanupMu.Lock()
		defer clientTempCleanupMu.Unlock()
		return clientTempCleanupAt
	}
	t.Cleanup(func() { reset(time.Time{}) })

	// ① 零值（首次调用）⇒ 立即执行清理并把时间戳推进
	reset(time.Time{})
	maybeCleanupClientTemp()
	first := readAt()
	if first.IsZero() {
		t.Fatal("首次调用应立即清理（时间戳应被设置）")
	}

	// ② 紧接着再调用 ⇒ **节流**（时间戳不变）
	maybeCleanupClientTemp()
	if !readAt().Equal(first) {
		t.Fatalf("节流窗口内不应重复清理：%v → %v", first, readAt())
	}

	// ③ 把时间戳拨回到超过间隔之前 ⇒ 再次执行
	reset(time.Now().Add(-clientTempCleanupInterval - time.Minute))
	before := readAt()
	maybeCleanupClientTemp()
	if !readAt().After(before) {
		t.Fatalf("超过间隔后应重新清理：%v", readAt())
	}
}

// TestClientTempDir_IsStableSubdir 锁定临时目录收敛到固定父目录（便于按目录批量清理）。
func TestClientTempDir_IsStableSubdir(t *testing.T) {
	t.Parallel()
	dir := clientTempDir()
	if filepath.Base(dir) != clientTempRoot {
		t.Fatalf("临时目录应为 <Temp>/%s，got %q", clientTempRoot, dir)
	}
	if _, err := os.Stat(dir); err != nil && !os.IsNotExist(err) {
		t.Fatalf("Stat 意外错误: %v", err)
	}
}

// TestStaleClientTempMaxAge_Reasonable 锁定孤儿阈值：不能太短（误删活跃文件的余量不足），
// 也不能太长（残留回收不及时）。当前取 1h。
func TestStaleClientTempMaxAge_Reasonable(t *testing.T) {
	t.Parallel()
	if staleClientTempMaxAge < 15*time.Minute || staleClientTempMaxAge > 6*time.Hour {
		t.Fatalf("staleClientTempMaxAge = %v，应在 [15m, 6h] 区间", staleClientTempMaxAge)
	}
	if clientTempCleanupInterval <= 0 || clientTempCleanupInterval > staleClientTempMaxAge {
		t.Fatalf("清理间隔 %v 应 > 0 且不大于阈值 %v", clientTempCleanupInterval, staleClientTempMaxAge)
	}
}

// TestMaybeCleanupClientTemp_RemovesAgedOrphanE2E 端到端锁定：**真的会删掉超龄孤儿**，
// 且**不删活跃文件**（mtime 新）。这是 2026-11-10「残留累积 64GiB」的直接回归测试。
//
// 安全性：只创建名字带本测试唯一前缀的文件；判据基于 mtime，活跃文件（刚写入）永不受影响。
func TestMaybeCleanupClientTemp_RemovesAgedOrphanE2E(t *testing.T) {
	// sproxy:serial: 需改写包内节流状态并触发真实清理，本质串行（非全局 seam 替换）。
	dir := clientTempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// ① 超龄孤儿：名字唯一，mtime 拨到阈值之外
	aged := filepath.Join(dir, "sproxy-sig-body-e2e-aged-"+t.Name())
	if err := os.WriteFile(aged, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用**固定的** 2 小时老化时间（不引用 staleClientTempMaxAge），
	// 否则"把阈值调大"这类变异会跟着改变夹具而失去检出能力（变异验证教训）。
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// ② 活跃文件：刚写入（mtime 新）⇒ 必须保留
	live := filepath.Join(dir, "sproxy-sig-body-e2e-live-"+t.Name())
	if err := os.WriteFile(live, []byte("y"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(aged); _ = os.Remove(live) })

	// 强制本次调用真的执行清理（把节流时间戳拨老）
	clientTempCleanupMu.Lock()
	clientTempCleanupAt = time.Now().Add(-clientTempCleanupInterval - time.Minute)
	clientTempCleanupMu.Unlock()
	maybeCleanupClientTemp()

	if _, err := os.Stat(aged); !os.IsNotExist(err) {
		t.Errorf("超龄孤儿应被删除（err=%v）", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("活跃文件不得被删除: %v", err)
	}
}
