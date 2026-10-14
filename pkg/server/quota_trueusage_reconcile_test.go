// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// quota_trueusage_reconcile_test.go —— 对账侧「真值优先」接线（设计 2026-10-11 §10/§12.1 第 2 项）：
// 卷后端 FS 若报告真值（内容分块 + meta pad + parity 的物理字节），卷容量池收敛到真值而非
// 磁盘扫描的估算值；无报告者时保持既有扫描口径（不静默降级为 0）。

// tuTruthFS 是只提供真值报告能力的假 FS（嵌入接口 → 未实现的 FS 方法不必写）。
type tuTruthFS struct {
	syncpkg.FS
	bytes int64
}

func (f *tuTruthFS) TrueUsage() (int64, bool)      { return f.bytes, true }
func (f *tuTruthFS) TrueSize(string) (int64, bool) { return f.bytes, true }

// tuPlainFS 不实现真值报告者（走扫描回落路径）。
type tuPlainFS struct{ syncpkg.FS }

// tuExternalBackend 是 ExternalBackend 的最小实现（只暴露 FS）。
type tuExternalBackend struct{ fs syncpkg.FS }

func (b tuExternalBackend) FS() syncpkg.FS { return b.fs }
func (b tuExternalBackend) Close() error   { return nil }

// newTruthReconcileHandlers 建一个只含单卷卷池 + 外部卷后端的 Handlers（不经完整装配）。
func newTruthReconcileHandlers(name string, be registry.ExternalBackend, maxBytes int64) (*Handlers, *quota.Pool) {
	pool := quota.NewPool(maxBytes)
	vs := registry.NewSet(
		[]volume.Volume{{Name: name}},
		map[string]*storage.Root{},
		map[string]registry.ExternalBackend{name: be},
		map[string]*quota.Pool{name: pool},
		name,
	)
	return &Handlers{volSet: vs, logger: testLogger()}, pool
}

// TestAdjustVolumePool_TruthBeatsScan 真值优先：扫描值 100（假）与真值 4096 不一致 → 卷池收敛到
// 真值（封装开销/pad/parity 不再被低估），而非扫描值。
func TestAdjustVolumePool_TruthBeatsScan(t *testing.T) {
	t.Parallel()
	h, pool := newTruthReconcileHandlers("truth", tuExternalBackend{fs: &tuTruthFS{bytes: 4096}}, 100000)

	h.adjustVolumePool("truth", map[string]map[string]int64{"alice": {"user": 100}})

	if got := pool.Usage(); got != 4096 {
		t.Fatalf("卷池 Usage = %d, want 4096（真值优先于扫描）", got)
	}
}

// TestAdjustVolumePool_NoReporterFallsBackToScan 无真值报告者（本地裸卷/未接线处理器）→ 保持既有
// 扫描口径（不得因「不掌握」而把账本清零）。
func TestAdjustVolumePool_NoReporterFallsBackToScan(t *testing.T) {
	t.Parallel()
	h, pool := newTruthReconcileHandlers("plain", tuExternalBackend{fs: &tuPlainFS{}}, 100000)

	h.adjustVolumePool("plain", map[string]map[string]int64{"alice": {"user": 100}})

	if got := pool.Usage(); got != 100 {
		t.Fatalf("卷池 Usage = %d, want 100（回落扫描值）", got)
	}
}

// TestVolumeTrueUsage_NoVolumeOrNoSet 未装配卷集 / 未知卷 → 无报告者（不 panic）。
func TestVolumeTrueUsage_NoVolumeOrNoSet(t *testing.T) {
	t.Parallel()
	var h Handlers
	if _, ok := h.volumeTrueUsage("x"); ok {
		t.Fatal("volSet 为 nil 时应无报告者")
	}
	h2, _ := newTruthReconcileHandlers("truth", tuExternalBackend{fs: &tuTruthFS{bytes: 1}}, 10)
	if _, ok := h2.volumeTrueUsage("other"); ok {
		t.Fatal("未知卷应无报告者")
	}
	if _, ok := h2.volumeTrueUsage("truth"); !ok {
		t.Fatal("已装配真值报告者的卷应返回报告者")
	}
}

// TestVolumeTruthTolerance 容差口径：绝对下限 1MiB 或扫描值 5% 取大者（真值口径不含目录 meta/
// 孤儿 blob，容差内差异不告警）。
func TestVolumeTruthTolerance(t *testing.T) {
	t.Parallel()
	if got := volumeTruthTolerance(0); got != 1<<20 {
		t.Fatalf("空卷容差 = %d, want 1MiB", got)
	}
	if got := volumeTruthTolerance(100 << 20); got != (100<<20)/20 {
		t.Fatalf("100MiB 容差 = %d, want 5MiB", got)
	}
	if got := volumeTruthTolerance(-1); got != 1<<20 {
		t.Fatalf("负值容差 = %d, want 1MiB（按 0 处理）", got)
	}
	if absInt64(-5) != 5 || absInt64(7) != 7 {
		t.Fatal("absInt64 语义错误")
	}
}
