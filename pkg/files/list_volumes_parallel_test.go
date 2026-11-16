// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// list_volumes_parallel_test.go 钉住**多卷列举并行**（性能项 P0.2）与**归并语义不变**：
//
// 背景：`collectVolumeViewEntries` 原为**逐卷串行** `ListDirView`；多本地卷 / 显式 `?volume=`
// 场景下延迟随卷数线性叠加（每卷一次 FS 列举 + S2 sidecar 补 checksum）。改为**有界并发**后，
// 延迟 ≈ 1/N；但**归并必须按 `vols` 顺序串行**，否则「目录跨卷去重（先到先得）」与输出顺序会漂移。
//
// 判据：用带**同步闸门**的替身 lister 观测并发峰值 ≥2（无需 time.Sleep），并断言
// 文件集合正确、目录跨卷去重仍只出现一次、每个文件带各自卷名。
//
// 变异：把实现改回串行 ⇒ 峰值 1 ⇒ 红。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// volGate 让**前 want 个** ListDirView 调用相互等待（之后立即放行）⇒ 确定性制造并发重叠。
type volGate struct {
	mu   sync.Mutex
	n    int
	want int
	ch   chan struct{}
}

func newVolGate(want int) *volGate { return &volGate{want: want, ch: make(chan struct{})} }

func (g *volGate) arrive() {
	g.mu.Lock()
	g.n++
	if g.n == g.want {
		close(g.ch)
	}
	ch := g.ch
	g.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(time.Second): // 上界：串行实现时避免死锁（非固定等待，R14 计数只看 time.Sleep）
	}
}

// gateLister 是 `VolumeViewLister` 替身：记录并发峰值 + 闸门 + 按卷返回固定条目。
type gateLister struct {
	gate     *volGate
	entries  map[string][]ExternalEntry
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (l *gateLister) ListDirView(_ context.Context, volName, _, _ string) ([]ExternalEntry, bool, error) {
	cur := l.inFlight.Add(1)
	defer l.inFlight.Add(-1)
	for {
		p := l.peak.Load()
		if cur <= p || l.peak.CompareAndSwap(p, cur) {
			break
		}
	}
	l.gate.arrive()
	return l.entries[volName], true, nil
}

// TestList_VolumeViewParallelAcrossVolumes 多卷列举并行，且归并语义不变。
func TestList_VolumeViewParallelAcrossVolumes(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")
	lister := &gateLister{
		gate: newVolGate(2), // 前 2 卷必须同时在列举 ⇒ 峰值 ≥2
		entries: map[string][]ExternalEntry{
			"main":  {{Name: "a.txt", Size: 1}, {Name: "dup", IsDir: true}},
			"disk2": {{Name: "b.txt", Size: 2}, {Name: "dup", IsDir: true}},
		},
	}
	env.viewList = lister
	env.rebuild()

	res, err := env.svc.List(t.Context(), ListQuery{Owner: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if peak := lister.peak.Load(); peak < 2 {
		t.Fatalf("多卷列举应并行（峰值 ≥2），实测 %d（退化回逐卷串行？）", peak)
	}
	// 归并语义：两卷文件都在且各带卷名；目录 dup 跨卷去重只出现一次。
	byName := map[string]FileInfo{}
	dupDirs := 0
	for _, f := range res.Files {
		if f.Name == "dup" {
			dupDirs++
			continue
		}
		byName[f.Name] = f
	}
	if dupDirs != 1 {
		t.Fatalf("目录 dup 应跨卷去重只出现一次，实测 %d 次：%+v", dupDirs, res.Files)
	}
	if f, ok := byName["a.txt"]; !ok || f.Volume != "main" || f.Size != 1 {
		t.Fatalf("a.txt 应来自 main 卷，got %+v", f)
	}
	if f, ok := byName["b.txt"]; !ok || f.Volume != "disk2" || f.Size != 2 {
		t.Fatalf("b.txt 应来自 disk2 卷，got %+v", f)
	}
}
