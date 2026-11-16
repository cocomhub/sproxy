// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rebuild_parallel_test.go 钉住**重建期 sidecar 读的有界并发**（性能项 P0.4）：
//
// 背景：`RebuildChecksums`（台账自愈 / 一次性导入，也可经 API/CLI 手动触发）是「全卷遍历 + 逐文件读
// sidecar」，原实现**串行**（实测 ≈27 µs/文件 ⇒ 10 万文件≈2.7 s）。与 P0（volumestate 冷填充）
// 同一读模式 ⇒ 有界并发（8）即可获得同量级加速。
//
// 判据：用带**同步闸门**的 meta 策略观测并发峰值 ≥2（无需 time.Sleep），并断言回填条数正确
// （并发不得丢条目）。
//
// 变异：把并发数改 1（退化回串行）⇒ 峰值 1 ⇒ 红。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// gateMetaPolicy 是观测型 meta 策略：记录 ReadMetaRaw 并发峰值 + 闸门；读取结果与测试替身一致。
type gateMetaPolicy struct {
	testMetaPolicy
	gate     *volGate
	inFlight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
}

func (p *gateMetaPolicy) ReadMetaRaw(ctx context.Context, root *storage.Root, mrel string) ([]byte, error) {
	cur := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		old := p.peak.Load()
		if cur <= old || p.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	p.calls.Add(1)
	p.gate.arrive()
	return p.testMetaPolicy.ReadMetaRaw(ctx, root, mrel)
}

// seedFileWithSidecar 在租户根写 `<rel>` 与它的 sidecar（含 `TotalSHA256`）。
func seedFileWithSidecar(t *testing.T, env *dirsEnv, rel, sum string) {
	t.Helper()
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("租户未装配")
	}
	root := tnt.Root()
	if err := root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", rel, err)
	}
	_, _ = f.Write([]byte("content-" + sum))
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	raw, merr := json.Marshal(meta.FileMeta{TotalSHA256: sum})
	if merr != nil {
		t.Fatalf("marshal meta: %v", merr)
	}
	mrel := meta.MetaPath(rel)
	if derr := root.MkdirAll(path.Dir(mrel), 0o755); derr != nil {
		t.Fatalf("MkdirAll(meta): %v", derr)
	}
	mf, err := root.OpenFile(mrel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", mrel, err)
	}
	_, _ = mf.Write(raw)
	if cerr := mf.Close(); cerr != nil {
		t.Fatalf("Close(meta): %v", cerr)
	}
}

// TestRebuildChecksums_ParallelSidecarReads 重建期 sidecar 读并行（峰值 ≥2）且条目不丢。
func TestRebuildChecksums_ParallelSidecarReads(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableVolumes(t, "default")
	env.enableWriteDefaults()
	pol := &gateMetaPolicy{gate: newVolGate(2)} // 前 2 次读必须重叠 ⇒ 峰值 ≥2
	env.metaPolicy = pol
	env.rebuild()

	const n = 8
	for i := range n {
		seedFileWithSidecar(t, env, fmt.Sprintf("user/docs/f%d.txt", i), fmt.Sprintf("sha%d", i))
	}

	got, err := env.svc.RebuildChecksums(t.Context(), "alice")
	if err != nil {
		t.Fatalf("RebuildChecksums: %v", err)
	}
	if got != n {
		t.Fatalf("回填条数 = %d, want %d（并发读不得丢条目）", got, n)
	}
	if peak := pol.peak.Load(); peak < 2 {
		t.Fatalf("重建期 sidecar 读应并行（峰值 ≥2），实测 %d（退化回串行？）", peak)
	}
}
