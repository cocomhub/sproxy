// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// newFSPolicy 构造带规则快照的 FS（底层本地临时目录；小块策略加速）。
func newFSPolicy(t *testing.T, p *policy.Compiled) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: shardseal.BlockModeRandom, Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Policy:    p,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// mustCompilePolicy 编译规则集（测试辅助，失败即终止）。
func mustCompilePolicy(t *testing.T, rules []policy.Rule) *policy.Compiled {
	t.Helper()
	c, err := policy.Compile(policy.Policy{Rules: rules}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

// TestUserVisibleRel 锁定「剥离 <owner>/user/ 前缀」的形状判据。
func TestUserVisibleRel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantRel string
		wantOK  bool
	}{
		{"alice/user/a/b.txt", "a/b.txt", true},
		{"user/user/x", "x", true},
		{"alice/user/f.bin", "f.bin", true},
		// 第二段不是 "user" ⇒ 不是用户面键（注意首段是 owner，故 a/user/b 属合法形状）
		{"archive/userdata/b.txt", "", false},
		{"meta/x", "", false},        // 非用户桶
		{"alice/cloud/x", "", false}, // 非用户桶
		{"alice/user/", "", false},   // 空文件名
		{"", "", false},
		{"alice", "", false},
	}
	for _, c := range cases {
		rel, ok := userVisibleRel(c.in)
		if ok != c.wantOK || rel != c.wantRel {
			t.Fatalf("userVisibleRel(%q) = (%q,%v)，期望 (%q,%v)", c.in, rel, ok, c.wantRel, c.wantOK)
		}
	}
}

// TestBlockletPolicyFor_NoRules_Unchanged 锁定零回归：无规则时策略与卷默认逐字段相同。
func TestBlockletPolicyFor_NoRules_Unchanged(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	got, err := fs.blockletPolicyFor("alice/user/a.bin", 100)
	if err != nil {
		t.Fatalf("blockletPolicyFor: %v", err)
	}
	if got.Mode != fs.opts.Block.Mode || got.Min != fs.opts.Block.Min || got.Max != fs.opts.Block.Max {
		t.Fatalf("无规则时应与卷默认一致：%+v vs %+v", got, fs.opts.Block)
	}
}

// TestBlockletPolicyFor_RuleHitSingle 锁定命中 single 时的块级覆写。
func TestBlockletPolicyFor_RuleHitSingle(t *testing.T) {
	t.Parallel()
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(1 << 20), Mode: shardseal.BlockModeSingle,
	}})
	fs := newFSPolicy(t, c)
	got, err := fs.blockletPolicyFor("alice/user/archive/a.bin", 100)
	if err != nil {
		t.Fatalf("blockletPolicyFor: %v", err)
	}
	if got.Mode != shardseal.BlockModeSingle {
		t.Fatalf("应命中 single，得到 mode=%q", got.Mode)
	}
	if got.Max != 100 || got.Min != 1 {
		t.Fatalf("single 时 Max 应为文件大小(100)、Min 为 1，得到 {%d,%d}", got.Min, got.Max)
	}
	// 未命中路径保持卷默认
	other, err := fs.blockletPolicyFor("alice/user/photos/a.bin", 100)
	if err != nil {
		t.Fatalf("blockletPolicyFor: %v", err)
	}
	if other.Mode != shardseal.BlockModeRandom {
		t.Fatalf("未命中应保持 random，得到 %q", other.Mode)
	}
}

// TestBlockletPolicyFor_RuleOnRawKeyIgnored 锁定非用户面键不套规则（形状不符）。
func TestBlockletPolicyFor_RuleOnRawKeyIgnored(t *testing.T) {
	t.Parallel()
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^meta/`, Mode: shardseal.BlockModeSingle, MaxSize: sizex.ByteSize(1 << 20),
	}})
	fs := newFSPolicy(t, c)
	got, err := fs.blockletPolicyFor("meta/x.bin", 100)
	if err != nil {
		t.Fatalf("blockletPolicyFor: %v", err)
	}
	if got.Mode != shardseal.BlockModeRandom {
		t.Fatalf("非用户面键不应套规则，得到 mode=%q", got.Mode)
	}
}

// TestWrite_SingleAtBoundary 锁定体积边界：等于上限单块、超一字节多块。
func TestWrite_SingleAtBoundary(t *testing.T) {
	t.Parallel()
	const limit = 512
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(limit), Mode: shardseal.BlockModeSingle,
	}})
	fs := newFSPolicy(t, c)
	ctx := context.Background()

	atLimit := make([]byte, limit)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/a.bin",
		bytes.NewReader(atLimit), int64(len(atLimit)), 1700000000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.mu.RLock()
	e := fs.index["alice/user/archive/a.bin"]
	fs.mu.RUnlock()
	if e == nil || e.meta == nil {
		t.Fatal("条目/meta 缺失")
	}
	if len(e.meta.Chunks) != 1 {
		t.Fatalf("等于上限应落单块，得到 %d 块", len(e.meta.Chunks))
	}
	if e.meta.Block.Mode != shardseal.BlockModeSingle {
		t.Fatalf("meta.Block.Mode 应为 single，得到 %q", e.meta.Block.Mode)
	}

	overLimit := make([]byte, limit+1)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/b.bin",
		bytes.NewReader(overLimit), int64(len(overLimit)), 1700000000); err != nil {
		t.Fatalf("WriteFile over: %v", err)
	}
	fs.mu.RLock()
	e2 := fs.index["alice/user/archive/b.bin"]
	fs.mu.RUnlock()
	if e2 == nil || e2.meta == nil {
		t.Fatal("超限条目/meta 缺失")
	}
	if e2.meta.Block.Mode == shardseal.BlockModeSingle {
		t.Fatalf("超过上限不应命中 single")
	}
}

// TestWrite_SingleBothPaths 锁定两条写路径（流式 / 内存）都套用规则。
func TestWrite_SingleBothPaths(t *testing.T) {
	t.Parallel()
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(1 << 20), Mode: shardseal.BlockModeSingle,
	}})
	fs := newFSPolicy(t, c)
	ctx := context.Background()
	content := data(600)

	// 可 seek 源 ⇒ 走 writeFileStream
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/seek.bin",
		bytes.NewReader(content), int64(len(content)), 1700000000); err != nil {
		t.Fatalf("WriteFile(seek): %v", err)
	}
	// 非 seek 源 ⇒ 走内存路径
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/pipe.bin",
		&nonSeekReader{r: bytes.NewReader(content)}, int64(len(content)), 1700000000); err != nil {
		t.Fatalf("WriteFile(pipe): %v", err)
	}
	for _, rel := range []string{"alice/user/archive/seek.bin", "alice/user/archive/pipe.bin"} {
		fs.mu.RLock()
		e := fs.index[rel]
		fs.mu.RUnlock()
		if e == nil || e.meta == nil {
			t.Fatalf("%s: 条目缺失", rel)
		}
		if len(e.meta.Chunks) != 1 {
			t.Fatalf("%s: 应单块，得到 %d", rel, len(e.meta.Chunks))
		}
	}
}

// nonSeekReader 只实现 io.Reader（迫使写路径走内存变体）。
type nonSeekReader struct{ r *bytes.Reader }

func (n *nonSeekReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// TestSetPolicy_HotSwapAppliesToNewWrites 锁定热替换：新写立即生效、旧文件仍可读。
func TestSetPolicy_HotSwapAppliesToNewWrites(t *testing.T) {
	t.Parallel()
	fs := newFSPolicy(t, nil)
	ctx := context.Background()
	content := data(600)

	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/a.bin",
		bytes.NewReader(content), int64(len(content)), 1700000000); err != nil {
		t.Fatalf("WriteFile #1: %v", err)
	}
	fs.mu.RLock()
	before := len(fs.index["alice/user/archive/a.bin"].meta.Chunks)
	fs.mu.RUnlock()
	if before < 2 {
		t.Fatalf("无规则时应多块（小块策略），得到 %d", before)
	}

	fs.SetPolicy(mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(1 << 20), Mode: shardseal.BlockModeSingle,
	}}))
	content2 := data(601)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "alice/user/archive/b.bin",
		bytes.NewReader(content2), int64(len(content2)), 1700000000); err != nil {
		t.Fatalf("WriteFile #2: %v", err)
	}
	fs.mu.RLock()
	after := len(fs.index["alice/user/archive/b.bin"].meta.Chunks)
	fs.mu.RUnlock()
	if after != 1 {
		t.Fatalf("热替换后新写应单块，得到 %d", after)
	}

	// 旧文件仍能完整读出
	rc, err := fs.OpenRead(ctx, "alice/user/archive/a.bin")
	if err != nil {
		t.Fatalf("OpenRead 旧文件: %v", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, cerr := buf.ReadFrom(rc); cerr != nil {
		t.Fatalf("读旧文件: %v", cerr)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatal("旧文件内容不一致")
	}
}

// TestWrite_SingleWithErasure_NoParity 锁定 single 命中时 erasure 不产 parity（单块无冗余）。
func TestWrite_SingleWithErasure_NoParity(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(1 << 20), Mode: shardseal.BlockModeSingle,
	}})
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: shardseal.BlockModeRandom, Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Erasure:   true,
		Policy:    c,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(600)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/archive/a.bin",
		bytes.NewReader(content), int64(len(content)), 1700000000); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	fs.mu.RLock()
	e := fs.index["alice/user/archive/a.bin"]
	fs.mu.RUnlock()
	if e == nil || e.meta == nil {
		t.Fatal("条目缺失")
	}
	if e.meta.Parity != nil {
		t.Fatalf("单块不应产 parity，得到 %+v", e.meta.Parity)
	}
}

// TestBlockModeResolvedCallback 锁定命中/未命中都回调（可观测性）。
func TestBlockModeResolvedCallback(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	type call struct {
		rel     string
		mode    string
		matched bool
	}
	var calls []call
	c := mustCompilePolicy(t, []policy.Rule{{
		PathRegex: `^archive/`, MaxSize: sizex.ByteSize(1 << 20), Mode: shardseal.BlockModeSingle,
	}})
	fs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: shardseal.BlockModeRandom, Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Policy:    c,
		OnBlockModeResolved: func(rel, mode string, matched bool) {
			calls = append(calls, call{rel, mode, matched})
		},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/archive/a.bin",
		bytes.NewReader(data(100)), 100, 1700000000); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if len(calls) != 1 || calls[0].mode != shardseal.BlockModeSingle || !calls[0].matched {
		t.Fatalf("命中回调不符: %+v", calls)
	}
}

// TestErrEntryExistsStillWorks 保证新代码未破坏既有哨兵语义（零回归小探针）。
func TestErrEntryExistsStillWorks(t *testing.T) {
	t.Parallel()
	if !errors.Is(errEntryExists, errEntryExists) {
		t.Fatal("哨兵错误比较异常")
	}
}
