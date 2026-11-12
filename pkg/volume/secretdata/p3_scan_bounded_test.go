// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"fmt"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestScanPath_MetaIsBoundedByL1B 锁定 P3 的核心不变量：
//
// **全量扫描是最大的批量 meta 读取场景**（实测单文件解密+解析 ≈ 584 ms，大文件 meta 常驻 78 MB）。
// 若把每条 meta 永久挂在 `metaEntry` 上，等于全量常驻（多文件可达数十 GB ⇒ OOM）。
// 正确行为：扫描仍然「即时可用」（e.meta 有值），但其**存活由 L1b 的字节预算决定** ——
// 超出预算的最久未用条目会被淘汰并**回清 e.meta**。
//
// 变异：删掉 registerLoadedEntryLocked 里的 clearEvictedMetaLocked/put ⇒ 本用例红。
func TestScanPath_MetaIsBoundedByL1B(t *testing.T) {
	// sproxy:serial: MemStats 语义无关，但该用例构造大量 meta 夹具，避免并行放大内存
	const perMeta = 1000 // blocklets
	oneMeta := metaSizeBytes(mkMeta(perMeta))

	// 预算只够 **2 条**。
	fs := &SecretdataFS{
		index:     map[string]*metaEntry{},
		dirs:      map[string]struct{}{},
		metaCache: newMetaEntryCache(oneMeta*2 + 1),
	}

	const n = 6
	for i := range n {
		rel := fmt.Sprintf("d/f%d.mp4", i)
		mm := mkMeta(perMeta)
		mm.Original = shardseal.OriginalInfo{
			Name: fmt.Sprintf("f%d", i), Size: 100, SHA256: fmt.Sprintf("sha%03d", i)}
		fs.mu.Lock()
		fs.registerLoadedEntryLocked(rel, "seg0", fmt.Sprintf("m%d", i), mm, "", 1, 8, nil)
		fs.mu.Unlock()
	}

	// 全部 6 条都应在索引里（条目本身不丢）。
	if len(fs.index) != n {
		t.Fatalf("应登记 %d 条，得到 %d", n, len(fs.index))
	}
	// 但**只有最近 2 条**的 meta 允许存活（预算约束）。
	alive := 0
	for rel, e := range fs.index {
		if e.meta != nil {
			alive++
		}
		// 热字段必须始终可用（不依赖 meta）。
		if e.origSHA256 == "" {
			t.Fatalf("%s: 热字段 origSHA256 缺失（checksum 会退化）", rel)
		}
	}
	if alive > 2 {
		t.Fatalf("存活 meta 数应受 L1b 预算约束（≤2），实际 %d ⇒ 扫描会全量常驻", alive)
	}
	if alive == 0 {
		t.Fatal("最近的条目应仍在缓存中（否则缓存完全失效）")
	}
	// 最久未用的第一条应已被淘汰并回清引用。
	if e := fs.index["d/f0.mp4"]; e != nil && e.meta != nil {
		t.Fatal("最早登记的条目应已被 L1b 淘汰并回清 meta")
	}
}

// TestScanPath_HotFieldAlwaysAvailable 锁定：扫描登记的条目**即使 meta 被淘汰**，
// 热字段（size/origSHA256/baseVersion/rel）仍然完整 ⇒ list/stat 不受影响。
func TestScanPath_HotFieldAlwaysAvailable(t *testing.T) {
	// sproxy:serial: 与上例同因（构造 meta 夹具）
	fs := &SecretdataFS{
		index:     map[string]*metaEntry{},
		dirs:      map[string]struct{}{},
		metaCache: newMetaEntryCache(metaSizeBytes(mkMeta(1000)) + 1),
	}
	mm := mkMeta(1000)
	mm.Original = shardseal.OriginalInfo{Name: "a.mp4", Size: 12345, SHA256: "abc123"}
	mm.BaseVersion = 7
	fs.mu.Lock()
	fs.registerLoadedEntryLocked("d/a.mp4", "seg0", "m0", mm, "", 99, 8, nil)
	// 再塞一条撑爆预算，把上一条挤出去（meta 被回清）。
	mm2 := mkMeta(1000)
	mm2.Original = shardseal.OriginalInfo{Name: "b.mp4", Size: 1}
	fs.registerLoadedEntryLocked("d/b.mp4", "seg0", "m1", mm2, "", 99, 8, nil)
	fs.mu.Unlock()

	e := fs.index["d/a.mp4"]
	if e == nil {
		t.Fatal("条目应存在")
	}
	if e.meta != nil {
		t.Fatal("被挤出的条目 meta 应已回清")
	}
	if e.size != 12345 || e.origSHA256 != "abc123" || e.baseVersion != 7 || e.rel != "d/a.mp4" {
		t.Fatalf("热字段必须完整（list/stat 不依赖 meta）: %+v", e)
	}
	if got := e.hotOrigSHA256(); got != "abc123" {
		t.Fatalf("checksum 应仍可从热字段给出，得到 %q", got)
	}
}
