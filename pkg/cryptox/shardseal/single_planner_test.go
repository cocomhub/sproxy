// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestSinglePlanner_Plan 锁定 single 模式把整文件规划为单块且覆盖 [0,size)。
func TestSinglePlanner_Plan(t *testing.T) {
	t.Parallel()
	const size = 1 << 20
	blocks, err := shardseal.SinglePlanner{}.Plan(size)
	if err != nil {
		t.Fatalf("Plan(%d): %v", size, err)
	}
	if len(blocks) != 1 {
		t.Fatalf("应产出 1 个块，得到 %d", len(blocks))
	}
	if blocks[0].Offset != 0 || blocks[0].Size != size {
		t.Fatalf("块描述应为 {0,%d}，得到 {%d,%d}", size, blocks[0].Offset, blocks[0].Size)
	}
}

// TestSinglePlanner_EmptyFailsClosed 锁定空/负长度与 RandomPlanner 同族 fail-closed。
func TestSinglePlanner_EmptyFailsClosed(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{0, -1} {
		if _, err := (shardseal.SinglePlanner{}).Plan(size); err == nil {
			t.Fatalf("size=%d 应 fail-closed", size)
		}
	}
}

// TestBlockPolicy_SingleMode_Planner 锁定 Planner() 装配：single 返回两个非 nil 规划器，
// 未知 mode 仍 fail-closed（返回 nil，与既有语义一致）。
func TestBlockPolicy_SingleMode_Planner(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{shardseal.BlockModeSingle} {
		bp, lp := shardseal.BlockPolicy{Mode: mode}.Planner()
		if bp == nil || lp == nil {
			t.Fatalf("mode=%q 应产出非 nil 规划器（bp=%v lp=%v）", mode, bp, lp)
		}
	}
	bp, lp := shardseal.BlockPolicy{Mode: "legend"}.Planner()
	if bp != nil || lp != nil {
		t.Fatal("未知 mode 应 fail-closed（返回 nil,nil）")
	}
}

// TestBlockPolicy_RandomModeConstant 锁定 BlockModeRandom 与既有 "random" 分支等价。
func TestBlockPolicy_RandomModeConstant(t *testing.T) {
	t.Parallel()
	a, b := shardseal.BlockPolicy{Mode: shardseal.BlockModeRandom}.Planner()
	c, d := shardseal.BlockPolicy{Mode: "random"}.Planner()
	if (a == nil) != (c == nil) || (b == nil) != (d == nil) {
		t.Fatal("BlockModeRandom 应与字面量 \"random\" 走同一分支")
	}
}

// TestSingleMode_DeterministicParity 锁定确定性：收敛档（默认算法）下同内容同策略两次加密
// 产出逐字节相同的 blob 与同名；与 random 模式的产物必须不同。
func TestSingleMode_DeterministicParity(t *testing.T) {
	t.Parallel()
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	data := make([]byte, 64<<10)
	for i := range data {
		data[i] = byte(i % 251)
	}
	dirA, dirB, dirC := t.TempDir(), t.TempDir(), t.TempDir()
	single := shardseal.BlockPolicy{
		Mode:         shardseal.BlockModeSingle,
		Min:          1,
		Max:          int64(len(data)),
		BlockletMode: shardseal.BlockletModeFixed,
		BlockletMin:  4096, BlockletMax: 16 << 10,
	}
	random := single
	random.Mode = shardseal.BlockModeRandom
	random.Min, random.Max = 4<<10, 8<<10

	a, err := shardseal.EncryptShardsBytes(data, "a.bin", dirA, secret, single, 0, shardseal.AlgoV2GCM)
	if err != nil {
		t.Fatalf("single #1: %v", err)
	}
	b, err := shardseal.EncryptShardsBytes(data, "a.bin", dirB, secret, single, 0, shardseal.AlgoV2GCM)
	if err != nil {
		t.Fatalf("single #2: %v", err)
	}
	if len(a.ChunkNames) != 1 {
		t.Fatalf("single 应只产 1 个分块，得到 %d", len(a.ChunkNames))
	}
	if a.ChunkNames[0] != b.ChunkNames[0] {
		t.Fatalf("同内容同策略两次加密的 blob 名不同：%q vs %q", a.ChunkNames[0], b.ChunkNames[0])
	}
	// meta 名不在此断言：EncryptShardsBytes 的 srcMeta.mtime 取 time.Now()（meta 内 Original.MTime
	// 参与 JSON ⇒ 名随 mtime 变），meta 名的确定性由 secretdata 层用**固定 mtime** 的
	// deterministic_parity_test.go 锁定。此处断言的是数据分块（blob 名 + 字节）逐字节相同。
	if !bytes.Equal(readRemote(t, dirA, a.ChunkNames[0]), readRemote(t, dirB, b.ChunkNames[0])) {
		t.Fatal("同内容同策略两次加密的分块字节不同")
	}
	c, err := shardseal.EncryptShardsBytes(data, "a.bin", dirC, secret, random, 0, shardseal.AlgoV2GCM)
	if err != nil {
		t.Fatalf("random: %v", err)
	}
	if len(c.ChunkNames) != 1 && c.ChunkNames[0] == a.ChunkNames[0] {
		t.Fatal("random 模式与 single 模式不应产出同名 blob")
	}
}

// readRemote 读取 outDir 内某个分块 blob 的字节（测试辅助）。
func readRemote(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读分块 %s: %v", name, err)
	}
	return b
}
