// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"path"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// trueusage_test.go —— 卷真值报告（capacity.TrueUsageReporter）的口径回归：
// **本卷落盘字节**（内容分块 EncSize + meta blob 含 pad 的落盘长度 + XOR parity EncSize
// + **目录 meta blob**），**不含副本**。钉住方式：与「底层 FS 实际落盘字节」逐字节比对。
//
// 目录 meta 自 2026-10-14（配额模型 v3）起**必须计入**：每目录 pad 到 [base, 2·base)（默认
// 1–2MB），若漏计，周期对账（TrueUsage → 卷池校准）会把已记账的目录字节改回去。

// diskDirMetaBytes 只统计底层 FS 中**目录 meta blob** 的落盘字节（逐文件 TrueSize 不含它们，
// 而卷真值含 ⇒ 断言需补上这一项）。
func diskDirMetaBytes(t *testing.T, inner syncpkg.FS) int64 {
	t.Helper()
	var total int64
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := inner.ListDir(context.Background(), dir)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name)
			if e.IsDir {
				walk(p)
				continue
			}
			if shardseal.ClassifyName(e.Name) == shardseal.KindDirMeta {
				total += e.Size
			}
		}
	}
	walk("")
	return total
}

// diskBytesAll 遍历底层 FS 统计落盘字节（含目录 meta——与真值口径一致）。
func diskBytesAll(t *testing.T, inner syncpkg.FS) int64 {
	t.Helper()
	var total int64
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := inner.ListDir(context.Background(), dir)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name)
			if e.IsDir {
				walk(p)
				continue
			}
			total += e.Size
		}
	}
	walk("")
	return total
}

// blobSize 返回底层某文件的落盘字节（找不到返回 -1）。
func blobSize(t *testing.T, inner syncpkg.FS, dir, name string) int64 {
	t.Helper()
	entries, err := inner.ListDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e.Size
		}
	}
	return -1
}

// TestTrueUsage_MatchesOnDiskBytes 常规（非去重）卷：真值 == 底层实际落盘字节（含目录 meta），
// 且逐文件 TrueSize 之和等于卷真值；小文件的物理字节必然大于逻辑字节（meta pad 计入）。
func TestTrueUsage_MatchesOnDiskBytes(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	rels := []string{"a.txt", "dir/b.bin", "dir/sub/c.dat"}
	for i, rel := range rels {
		data := bytes.Repeat([]byte{byte('a' + i)}, 300+i*17)
		if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel, bytes.NewReader(data), int64(len(data)), 0); err != nil {
			t.Fatalf("写 %s: %v", rel, err)
		}
	}

	truth, ok := fs.TrueUsage()
	if !ok {
		t.Fatal("secretdata 层必须报告 ok=true")
	}
	disk := diskBytesAll(t, fs.inner)
	if truth != disk {
		t.Fatalf("真值 = %d, 底层落盘(含目录 meta) = %d，应逐字节一致", truth, disk)
	}

	var sum int64
	for _, rel := range rels {
		n, tok := fs.TrueSize(rel)
		if !tok {
			t.Fatalf("TrueSize(%q) 应命中索引", rel)
		}
		if n <= 0 {
			t.Fatalf("TrueSize(%q) = %d，物理字节应为正", rel, n)
		}
		sum += n
	}
	// 逐文件 TrueSize 只含文件自身（内容 + parity + 文件 meta）；卷真值还含**目录 meta**。
	if dirMeta := diskDirMetaBytes(t, fs.inner); sum+dirMeta != truth {
		t.Fatalf("逐文件物理之和 %d + 目录 meta %d = %d, 卷真值 = %d（非去重卷应一致）",
			sum, dirMeta, sum+dirMeta, truth)
	}
}

// TestTrueUsage_ErasureIncludesParity Erasure 卷：真值包含 XOR parity 的落盘字节
// （只算内容+meta 会低估，正是「逻辑记账 ≠ 物理占用」的失真来源之一）。
func TestTrueUsage_ErasureIncludesParity(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	fs.opts.Erasure = true
	data := bytes.Repeat([]byte("parity-payload-"), 400) // 小块策略 → 多分块
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "p.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("写: %v", err)
	}
	e := fs.index["p.bin"]
	if e == nil || e.meta == nil {
		t.Fatal("索引条目缺失")
	}
	if e.meta.Parity == nil || len(e.meta.Chunks) < 2 {
		t.Fatalf("应生成 parity 且 ≥2 分块（chunks=%d parity=%v）", len(e.meta.Chunks), e.meta.Parity)
	}

	truth, _ := fs.TrueUsage()
	if disk := diskBytesAll(t, fs.inner); truth != disk {
		t.Fatalf("真值 = %d, 底层落盘 = %d（应含 parity）", truth, disk)
	}
	// 内容 + meta（不含 parity）必然小于真值 ⇒ 证明 parity 被计入。
	metaBlobLen := blobSize(t, fs.inner, e.dirSeg, e.metaName)
	if metaBlobLen < 0 {
		t.Fatalf("未找到 meta blob %s/%s", e.dirSeg, e.metaName)
	}
	var contentOnly int64
	for _, c := range e.meta.Chunks {
		contentOnly += c.EncSize
	}
	if truth <= contentOnly+metaBlobLen {
		t.Fatalf("真值 %d 应大于「内容 %d + meta %d」（差值为 parity %d）",
			truth, contentOnly, metaBlobLen, e.meta.Parity.EncSize)
	}
}

// TestTrueUsage_DedupCountsSharedOnce 去重卷：共享内容只计一次（两条目共享同一内容 blob，
// 只多出各自 meta），真值仍等于底层落盘字节——若按条目重复计内容会虚高。
func TestTrueUsage_DedupCountsSharedOnce(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	fs.opts.Dedup = true
	payload := bytes.Repeat([]byte("same-content-for-dedup"), 300)
	for _, rel := range []string{"d1.bin", "d2.bin"} {
		if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel, bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
			t.Fatalf("写 %s: %v", rel, err)
		}
	}
	if len(fs.dedupPool) != 1 {
		t.Fatalf("去重池条目 = %d, want 1", len(fs.dedupPool))
	}
	truth, _ := fs.TrueUsage()
	if disk := diskBytesAll(t, fs.inner); truth != disk {
		t.Fatalf("去重卷真值 = %d, 底层落盘 = %d（共享内容只应计一次）", truth, disk)
	}
	// 两个引用条目的 TrueSize 只含各自 meta（共享内容不摊派）。
	var metas int64
	for _, rel := range []string{"d1.bin", "d2.bin"} {
		n, _ := fs.TrueSize(rel)
		metas += n
	}
	if metas >= truth {
		t.Fatalf("引用条目 meta 合计 %d 应小于卷真值 %d（共享内容按池条目计一次）", metas, truth)
	}
	dirMeta := diskDirMetaBytes(t, fs.inner)
	if truth != metas+contentPhysBytes(fs.dedupPool[keyOfDedup(fs)].meta)+dirMeta {
		t.Fatalf("真值 = %d, want 条目 meta 合计 %d + 共享内容 %d + 目录 meta %d", truth, metas,
			contentPhysBytes(fs.dedupPool[keyOfDedup(fs)].meta), dirMeta)
	}
}

// keyOfDedup 返回去重池唯一键（测试辅助：池内此时只应有一条）。
func keyOfDedup(fs *SecretdataFS) string {
	for k := range fs.dedupPool {
		return k
	}
	return ""
}

// TestTrueUsage_AfterReloadMatches 重启（重新加载索引）后真值不变——加载路径必须重算物理字节，
// 否则「重启后真值归零/漏算」会让对账把卷池压回逻辑值。
func TestTrueUsage_AfterReloadMatches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	}
	fs1, err := NewFS(syncpkg.NewLocalFS(root, nil), opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	payload := bytes.Repeat([]byte("reload-truth"), 250)
	if werr := fs1.WriteFile(context.Background(), syncpkg.LogicalKey{}, "r.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("写: %v", werr)
	}
	before, _ := fs1.TrueUsage()
	if before <= 0 {
		t.Fatalf("真值应为正, got %d", before)
	}

	fs2, err := NewFS(syncpkg.NewLocalFS(root, nil), opts)
	if err != nil {
		t.Fatalf("重开 NewFS: %v", err)
	}
	after, ok := fs2.TrueUsage()
	if !ok {
		t.Fatal("重开后应仍报告 ok=true")
	}
	if after != before {
		t.Fatalf("重载后真值 = %d, 重载前 = %d（加载路径应重算物理字节）", after, before)
	}
	if _, tok := fs2.TrueSize("r.bin"); !tok {
		t.Fatal("重载后 TrueSize 应命中")
	}
}

// TestTrueSize_UnknownRel 未知路径必须报「不掌握」（ok=false），不得推 0。
func TestTrueSize_UnknownRel(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	if n, ok := fs.TrueSize("nope.bin"); ok || n != 0 {
		t.Fatalf("未知 rel 应 (0,false), got (%d,%v)", n, ok)
	}
	if _, ok := fs.TrueUsage(); !ok {
		t.Fatal("空卷也应报告 ok=true（真值为 0 是有效真值）")
	}
}

// TestPhysBytesOf 纯函数口径：内容分块 EncSize 之和 + parity + meta blob；去重条目只计 meta。
func TestPhysBytesOf(t *testing.T) {
	t.Parallel()
	mm := &shardseal.Meta{
		Chunks: []shardseal.ChunkInfo{{EncSize: 10}, {EncSize: 20}},
		Parity: &shardseal.ParityInfo{EncSize: 5},
	}
	if got := physBytesOf(mm, 300, false); got != 335 {
		t.Fatalf("physBytesOf = %d, want 335（10+20+5+300）", got)
	}
	if got := physBytesOf(mm, 300, true); got != 300 {
		t.Fatalf("去重条目 physBytesOf = %d, want 300（共享内容不摊派）", got)
	}
	if got := physBytesOf(nil, 7, false); got != 7 {
		t.Fatalf("nil meta physBytesOf = %d, want 7（只计 meta blob）", got)
	}
	if got := contentPhysBytes(mm); got != 35 {
		t.Fatalf("contentPhysBytes = %d, want 35（10+20+5，不含 meta）", got)
	}
}
