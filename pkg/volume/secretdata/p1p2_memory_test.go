// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
)

// heapAllocBytes 返回当前存活对象占用（GC 后）。测试内存行为必须用 HeapAlloc——
// HeapInuse 会被归还 OS；且 MemStats 度量与并行互斥 ⇒ 用例内标 serial。
func heapAllocBytes() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestP1P2_SnapshotLoadKeepsMemoryBounded 量化 P1+P2 的**核心收益**：
//
// 夹具：**20 个条目，每个 meta 含 20 万个 blocklet**（真实卷里单文件实测可达 43 万个，
// 单条常驻 78 MB）。旧行为下这些 meta 会随快照一起进内存 ⇒ 20 × ~38 MB ≈ 760 MB+；
// P1 后快照**不承载 meta**，加载后 `meta == nil` ⇒ 常驻量应只与**热字段**相关。
//
// 判据有决定性：断言①快照落盘体积远小于 meta 总量；②apply 后堆增量 < 16 MiB
// （热字段 20 条 ≈ 几 KB；留足余量以抗测试框架噪声）。
func TestP1P2_SnapshotLoadKeepsMemoryBounded(t *testing.T) {
	// sproxy:serial: MemStats 度量需独占（GC 全局暂停），与并行用例互斥
	dir := t.TempDir()
	path := dir + "/vol.idx"
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path},
		index: map[string]*metaEntry{}}

	const nEntries, nBlocklets = 20, 200000
	for i := range nEntries {
		rel := fmt.Sprintf("d%d/f%d.mp4", i%5, i)
		e := &metaEntry{
			size: 1 << 30, mtime: 1700000000, dirSeg: fmt.Sprintf("seg%d", i%5),
			metaName: fmt.Sprintf("m%04d", i), meta: bigMeta(nBlocklets),
			origSHA256: fmt.Sprintf("sha%06d", i), baseVersion: 1, phys: 1 << 20, rel: rel,
		}
		fs.index[rel] = e
	}
	// 快照只带热字段 ⇒ 体积应与 meta 总量脱钩。
	snap := fs.buildIndexSnapshot([]string{"seg0"})
	fs.writeIndexSnapshot(snap)
	var sz int64
	if st, err := os.Stat(path); err == nil {
		sz = st.Size()
	}
	if sz > 256<<10 {
		t.Fatalf("快照应只含热字段（%d 条），实际 %d 字节", nEntries, sz)
	}

	// 模拟重启：加载快照到全新实例，测量**堆增量**。
	fresh := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	loaded, ok := fresh.loadIndexSnapshot()
	if !ok {
		t.Fatal("快照应能载入")
	}
	before := heapAllocBytes()
	fresh.applyIndexSnapshot(loaded)
	after := heapAllocBytes()
	grew := int64(after) - int64(before)
	// 参照：若 meta 随快照进内存，20 × 200k blocklets ≈ 20 × 38 MB ≈ 760 MB。
	if grew > 16<<20 {
		t.Fatalf("应用快照后堆增量应 < 16 MiB（只留热字段），实际 %.1f MiB", float64(grew)/(1<<20))
	}
	// 反向断言：条目确实被恢复（避免"什么都没做所以很省"的假绿）。
	if len(fresh.index) != nEntries {
		t.Fatalf("应恢复 %d 条，得到 %d", nEntries, len(fresh.index))
	}
	for _, e := range fresh.index {
		if e.meta != nil {
			t.Fatal("加载后 meta 必须为 nil（P1）")
		}
	}
	// 参照量（仅日志，便于人工核对规模）：meta 若进内存会占多少。
	rawOne, _ := json.Marshal(bigMeta(nBlocklets))
	t.Logf("参照：单条 meta JSON ≈ %.1f MB；%d 条若常驻 ≈ %.0f MB；实测堆增量 %.2f MiB",
		float64(len(rawOne))/(1<<20), nEntries,
		float64(len(rawOne))*nEntries/(1<<20), float64(grew)/(1<<20))
}
