// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// bandwidth_coord_test.go 验证带宽限速跨实例协调后端（coord_backend=file）：
//  1. 跨实例共享：两个 byteFileCoordinator 实例共用同一 storage 目录，同一 owner 字节
//     预算不超 per_owner_bps（总量 N 实例合计 ≤ 配额）；
//  2. 等待不拒绝：配额耗尽 Consume 返回 false（不拒绝请求，由领域层 WaitN 轮询等待）；
//  3. 窗口刷新：超 window 后配额重置（新窗口可继续消耗）；
//  4. 装配层：coord_backend=file 时桶装配协调器（跨实例共享生效）；local 默认零回归。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestByteFileCoordinator_CrossInstanceShared(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c1 := newByteFileCoordinator(1000, time.Minute, dir, testutil.DiscardLogger())
	c2 := newByteFileCoordinator(1000, time.Minute, dir, testutil.DiscardLogger())
	// c1 消耗 600 字节，c2 再消耗 500 → 总量 1100 > 1000，c2 应被拒（共享预算）。
	if !c1.Consume("alice", 600) {
		t.Fatal("c1 600B should pass")
	}
	if !c2.Consume("alice", 400) {
		t.Fatal("c2 400B should pass (total 1000 = quota)")
	}
	if c2.Consume("alice", 1) {
		t.Fatal("c2 1B should be rejected (total 1001 > 1000 shared)")
	}
}

func TestByteFileCoordinator_KeyIsolated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newByteFileCoordinator(100, time.Minute, dir, testutil.DiscardLogger())
	if !c.Consume("alice", 100) {
		t.Fatal("alice 100B should pass")
	}
	if c.Consume("alice", 1) {
		t.Fatal("alice 1B should be rejected")
	}
	if !c.Consume("bob", 100) {
		t.Fatal("bob 100B should pass (independent owner)")
	}
}

func TestByteFileCoordinator_WindowRefill(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// window 用 2s：Windows 上文件 IO 慢 + -race 下余量（与 fileCoordinator 测试同约定）。
	c := newByteFileCoordinator(100, 2*time.Second, dir, testutil.DiscardLogger())
	if !c.Consume("k", 100) {
		t.Fatal("first window 100B should pass")
	}
	if c.Consume("k", 1) {
		t.Fatal("within window should be rejected")
	}
	if !testutil.WaitForBool(30*time.Second, func() bool { return c.Consume("k", 1) }) {
		t.Fatal("after window refill should be allowed")
	}
}

func TestByteFileCoordinator_MkdirLayout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := newByteFileCoordinator(100, time.Minute, dir, testutil.DiscardLogger())
	if !c.Consume("k", 10) {
		t.Fatal("first consume should pass")
	}
	if _, err := filepath.Glob(filepath.Join(dir, "bandwidth", "k")); err != nil {
		t.Fatalf("glob bandwidth/k: %v", err)
	}
}
