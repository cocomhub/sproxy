// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// physfootprint_test.go 钉住 secretdata 卷的**物理足迹**口径（配额模型 v3：只按物理占用记账）。
//
// 注：空文件（size=0）本卷不支持（shardseal 拒绝空分块），故用例从 1KB 起。
//
// 决定性事实：meta blob **pad 到 [base, 2·base)**（默认 base = Block.Min = 1MB），与文件逻辑
// 大小**无关** ⇒ 按逻辑长度记账在小文件/多目录场景下物理可达逻辑的千倍。
// 因此本文件锁两条：
//  1. `PhysBound(logical) >= 实测物理`（预占上界必须**安全**：宁大勿小，结算按实测收敛）；
//  2. 小文件的实测物理 **≫ 逻辑**（meta pad 被计入）⇒ 若有人把 footprint 改回逻辑长度，此测红。

func physEnv(t *testing.T, opts Options) *SecretdataFS {
	t.Helper()
	opts.Secret = []byte("phys-test-key-0123456789")
	if opts.TempDir == "" {
		opts.TempDir = t.TempDir()
	}
	if opts.Block.Min == 0 {
		opts.Block = shardseal.DefaultBlockPolicy()
	}
	fs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

func physWrite(t *testing.T, fs *SecretdataFS, rel string, n int) {
	t.Helper()
	ctx := context.Background()
	if err := fs.WriteFile(ctx, rel, bytes.NewReader(bytes.Repeat([]byte("q"), n)), int64(n), 0); err != nil {
		t.Fatalf("WriteFile %s(%d): %v", rel, n, err)
	}
}

// TestPhysBound_CoversActual：预占上界必须覆盖实测物理（含 meta pad / parity）。
func TestPhysBound_CoversActual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    Options
		sizes   []int
		erasure bool
	}{
		{name: "默认策略（无 erasure）", opts: Options{MetaPadBytes: 1 << 20}, sizes: []int{1024, 512 << 10, 4 << 20}},
		{name: "小分块（强制多分块）", opts: Options{MetaPadBytes: 64 << 10,
			Block: shardseal.BlockPolicy{Mode: "random", Min: 64 << 10, Max: 128 << 10,
				BlockletMin: 16 << 10, BlockletMax: 32 << 10}},
			sizes: []int{1024, 256 << 10, 1 << 20}},
		{name: "erasure 开（parity 上界）", opts: Options{MetaPadBytes: 64 << 10, Erasure: true,
			Block: shardseal.BlockPolicy{Mode: "random", Min: 64 << 10, Max: 128 << 10,
				BlockletMin: 16 << 10, BlockletMax: 32 << 10}},
			sizes: []int{1024, 256 << 10, 1 << 20}, erasure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := physEnv(t, tc.opts)
			for i, n := range tc.sizes {
				rel := "f" + string(rune('a'+i)) + ".bin"
				physWrite(t, fs, rel, n)
				actual, ok := fs.PhysFootprint(rel)
				if !ok || actual <= 0 {
					t.Fatalf("PhysFootprint(%s)=%d ok=%v（应实测出物理字节）", rel, actual, ok)
				}
				if v, o := fs.TrueSize(rel); !o || v != actual {
					t.Fatalf("PhysFootprint 必须与 TrueSize 同源: %d vs %d(ok=%v)", actual, v, o)
				}
				bound := fs.PhysBound(int64(n))
				if bound < actual {
					t.Fatalf("预占上界不足（bound=%d < 实测=%d，logical=%d）⇒ 会超真实配额", bound, actual, n)
				}
			}
		})
	}
}

// TestPhysFootprint_SmallFileIncludesMetaPad：小文件物理 ≫ 逻辑（meta pad 计入）——本测即
// 「按逻辑记账」缺陷的回归钉。
func TestPhysFootprint_SmallFileIncludesMetaPad(t *testing.T) {
	t.Parallel()
	const base = 1 << 20 // MetaPadBytes = 1MB ⇒ meta blob pad 到 [1MB, 2MB)
	fs := physEnv(t, Options{MetaPadBytes: base})
	physWrite(t, fs, "tiny.bin", 1024)
	actual, ok := fs.PhysFootprint("tiny.bin")
	if !ok {
		t.Fatal("PhysFootprint 不可用")
	}
	if actual < base {
		t.Fatalf("1KB 文件物理 = %d，应 ≥ meta pad 下限 %d（按逻辑记账会低估千倍）", actual, base)
	}
	if bound := fs.PhysBound(1024); bound < actual {
		t.Fatalf("上界 %d < 实测 %d", bound, actual)
	}
	// 小文件的物理 / 逻辑 比值必须远大于 1（证明口径是物理而非逻辑）。
	if ratio := float64(actual) / 1024; ratio < 100 {
		t.Fatalf("物理/逻辑 = %.1f，应 ≫ 1（meta pad 未被计入？）", ratio)
	}
}

// TestDirPhysFootprint_MeasuredAndBounded：目录 meta blob 同样 pad 到 [base, 2·base) 且可实测。
func TestDirPhysFootprint_MeasuredAndBounded(t *testing.T) {
	t.Parallel()
	const base = 1 << 20
	fs := physEnv(t, Options{MetaPadBytes: base})
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "videos"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	actual, ok := fs.DirPhysFootprint(ctx, "videos")
	if !ok || actual <= 0 {
		t.Fatalf("DirPhysFootprint=%d ok=%v（应实测出目录 meta 物理字节）", actual, ok)
	}
	if actual < base {
		t.Fatalf("目录 meta 物理 = %d，应 ≥ pad 下限 %d", actual, base)
	}
	if bound := fs.DirPhysBound(); bound < actual {
		t.Fatalf("目录上界 %d < 实测 %d", bound, actual)
	}
	if _, ok := fs.DirPhysFootprint(ctx, "not-exist"); ok {
		t.Fatal("不存在的目录应 ok=false")
	}
}

// TestDirChainFootprint_MultiLevelDiff：递归 mkdir 的**链差分** = 本次新建目录的物理字节（含祖先）。
// 单看叶子会漏记祖先链（每级一个 dir meta blob）⇒ 差分是「mkdir/rmdir 精确记账」的基础。
func TestDirChainFootprint_MultiLevelDiff(t *testing.T) {
	t.Parallel()
	const base = 64 << 10
	fs := physEnv(t, Options{MetaPadBytes: base})
	ctx := context.Background()
	before, ok := fs.DirChainFootprint(ctx, "a/b/c")
	if !ok {
		t.Fatal("DirChainFootprint 应恒 ok=true（本层支持目录足迹）")
	}
	if err := fs.MakeDir(ctx, "a/b/c"); err != nil {
		t.Fatalf("MakeDir a/b/c: %v", err)
	}
	after, _ := fs.DirChainFootprint(ctx, "a/b/c")
	if created := after - before; created < 3*base {
		t.Fatalf("新建 a/b/c 应含 3 级 dir meta（每级 ≥%d），got %d", base, created)
	}
	// 单级新增：差分应落在一个 dir meta 的 [base, 2·base] 区间内。
	if err := fs.MakeDir(ctx, "a/b/c/d"); err != nil {
		t.Fatalf("MakeDir a/b/c/d: %v", err)
	}
	after2, _ := fs.DirChainFootprint(ctx, "a/b/c/d")
	if d := after2 - after; d < base || d > 2*base+blobFormatSlack {
		t.Fatalf("单级新增差分 = %d，应在一个 dir meta 上界内（[%d, %d]）", d, base, 2*base+blobFormatSlack)
	}
	// rmdir 后链足迹必须下降（释放侧同口径）。
	if err := fs.RemoveDir(ctx, "a/b/c/d"); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}
	after3, _ := fs.DirChainFootprint(ctx, "a/b/c/d")
	if after3 >= after2 {
		t.Fatalf("rmdir 后链足迹应下降: %d → %d", after2, after3)
	}
}
