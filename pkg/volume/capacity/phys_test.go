// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// physMapFS 在 mapFS 之上模拟「逻辑 ≠ 物理」的卷（加密卷：chunk 头 + meta pad + parity）。
//
// 物理字节 = 逻辑 × 2（parity）+ 1024（每文件 pad）——与 secretdata 的形态同构，便于断言。
type physMapFS struct {
	*mapFS
}

func (f *physMapFS) PhysFootprint(rel string) (int64, bool) {
	s, ok := f.read(rel)
	if !ok {
		return 0, false
	}
	return int64(len(s))*2 + 1024, true
}

func (f *physMapFS) PhysBound(logical int64) int64 { return logical*2 + 1024 }

// newPhysAccountFS 构造物理口径的 CapacityFS（真实 LedgerCounter）。
func newPhysAccountFS(t *testing.T, max int64) (*CapacityFS, *LedgerCounter) {
	t.Helper()
	led, err := OpenLedger(t.TempDir(), "phys")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, max)
	return Wrap(&physMapFS{mapFS: &mapFS{files: map[string]string{}}}, c), c
}

// TestCapacityFS_PhysicalAccounting 钉住用户裁定（2026-10-29）：卷容量按**实际占用**记账——
// 有物理足迹能力的卷（加密卷）不得按逻辑字节计，否则小文件可绕过卷上限约千倍。
//
// 变异：`physSize` 改回 `innerSize`（逻辑）⇒ 三处断言全红。
func TestCapacityFS_PhysicalAccounting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs, c := newPhysAccountFS(t, 0)

	// ① 写 100B（物理 = 100*2+1024 = 1224）。
	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.Used(); got != 1224 {
		t.Fatalf("新建文件的账本占用 = %d，want 1224（物理：2×逻辑+1024 pad）", got)
	}

	// ② 覆盖写 200B（物理 = 1424）⇒ 账本随实测收敛。
	if err := writeN(t, fs, "a.bin", 200); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := c.Used(); got != 1424 {
		t.Fatalf("覆盖后账本占用 = %d，want 1424", got)
	}

	// ③ 删除按物理释放。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := c.Used(); got != 0 {
		t.Fatalf("删除后账本占用 = %d，want 0（须按物理释放）", got)
	}
}

// TestCapacityFS_PhysicalBoundRejectsBeforeWrite 钉住写前预留按**物理上界** fail-closed：
// 逻辑 100B 在物理上要 1224B，卷上限 1200 ⇒ 必须拒绝（按逻辑判会误放行）。
func TestCapacityFS_PhysicalBoundRejectsBeforeWrite(t *testing.T) {
	t.Parallel()
	fs, _ := newPhysAccountFS(t, 1200)

	err := writeN(t, fs, "a.bin", 100)
	if err == nil {
		t.Fatal("逻辑 100B（物理 1224B）超过上限 1200 应被拒绝")
	}
	if !errors.Is(err, ErrVolumeFull) {
		t.Fatalf("错误应为 ErrVolumeFull，got %v", err)
	}
}

// TestCapacityFS_NoPhysCapabilityKeepsLogical 零回归：无物理能力的卷仍按逻辑字节记账
// （普通本地/外部卷行为不变）。
func TestCapacityFS_NoPhysCapabilityKeepsLogical(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 0)
	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := c.Used(); got != 100 {
		t.Fatalf("无物理能力应按逻辑 100 记账，got %d", got)
	}
	if err := writeN(t, fs, "b.bin", int64(len(strings.Repeat("y", 50)))); err != nil {
		t.Fatalf("write b: %v", err)
	}
}
