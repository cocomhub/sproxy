// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package policy_test

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// TestCompile_Validation 覆盖 Compile 的每条 fail-closed 判据（表驱动）。
func TestCompile_Validation(t *testing.T) {
	t.Parallel()
	ok20 := sizex.ByteSize(20 << 20)
	cases := []struct {
		name    string
		rules   []policy.Rule
		volMax  int64
		wantErr string
	}{
		{"valid single", []policy.Rule{{PathRegex: `^archive/.*\.zip$`, MaxSize: ok20, Mode: "single"}}, 0, ""},
		{"valid random", []policy.Rule{{PathRegex: `\.jpg$`, Mode: "random"}}, 0, ""},
		{"empty rules ok", nil, 0, ""},
		{"empty regex", []policy.Rule{{PathRegex: ``, Mode: "random"}}, 0, "规则 0"},
		{"regex too long", []policy.Rule{{PathRegex: strings.Repeat("a", policy.MaxRegexLen+1), Mode: "random"}}, 0, "规则 0"},
		{"single without max", []policy.Rule{{PathRegex: `x`, Mode: "single"}}, 0, "需给 max_size"},
		{"single over hard cap", []policy.Rule{{PathRegex: `x`, MaxSize: sizex.ByteSize(policy.HardMaxSingleBytes + 1), Mode: "single"}}, 0, "硬上限"},
		{"single over volume max", []policy.Rule{{PathRegex: `x`, MaxSize: ok20, Mode: "single"}}, 1 << 20, "卷块上限"},
		{"single equals volume max", []policy.Rule{{PathRegex: `x`, MaxSize: ok20, Mode: "single"}}, 20 << 20, ""},
		{"bad regex", []policy.Rule{{PathRegex: `(`, Mode: "random"}}, 0, "规则 0"},
		{"unknown mode", []policy.Rule{{PathRegex: `x`, Mode: "legend"}}, 0, "规则 0"},
		{"min>max", []policy.Rule{{PathRegex: `x`, MinSize: ok20, MaxSize: sizex.ByteSize(1), Mode: "random"}}, 0, "规则 0"},
		{"too many rules", make([]policy.Rule, policy.MaxRules+1), 0, "条数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := policy.Compile(policy.Policy{Rules: c.rules}, c.volMax)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("不应报错，得到: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("应报错（含 %q），但通过", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("错误信息应含 %q，得到: %v", c.wantErr, err)
			}
		})
	}
}

// TestCompiled_Resolve_SizeBoundary 锁定体积闭区间：等于上限命中，超一字节不命中。
func TestCompiled_Resolve_SizeBoundary(t *testing.T) {
	t.Parallel()
	const limit = int64(20 << 20)
	c, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `^archive/.*`, MaxSize: sizex.ByteSize(limit), Mode: "single", Priority: 100},
	}}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	cases := []struct {
		name string
		rel  string
		size int64
		want bool
	}{
		{"exact limit hits", "archive/a.zip", limit, true},
		{"over limit misses", "archive/a.zip", limit + 1, false},
		{"under limit hits", "archive/a.zip", limit - 1, true},
		{"path miss", "photos/a.zip", 1024, false},
		{"prefix must be whole", "archiveX/a.zip", 1024, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, got := c.Resolve(tc.rel, tc.size)
			if got != tc.want {
				t.Fatalf("Resolve(%q,%d) = %v，期望 %v", tc.rel, tc.size, got, tc.want)
			}
		})
	}
}

// TestCompiled_Resolve_PriorityAndOrder 锁定排序：priority 降序，同 priority 保持声明序，
// 首命中即停（低优先级但更宽的规则不会覆盖高优先级命中）。
func TestCompiled_Resolve_PriorityAndOrder(t *testing.T) {
	t.Parallel()
	c, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `.*`, Mode: "random", Priority: 1, Note: "wide-low"},
		{PathRegex: `^archive/`, Mode: "random", Priority: 100, Note: "narrow-high"},
		{PathRegex: `^archive/`, Mode: "random", Priority: 100, Note: "narrow-high-second"},
		{PathRegex: `^archive/.*\.zip$`, Mode: "single", MaxSize: 20 << 20, Priority: 100, Note: "narrow-high-third"},
	}}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got, ok := c.Resolve("archive/a.bin", 64)
	if !ok {
		t.Fatal("应命中")
	}
	// 同 priority 时声明序在前者胜 ⇒ "narrow-high"（不是 third，也不是 wide-low）
	if got.Note != "narrow-high" {
		t.Fatalf("同 priority 应按声明序取首条，得到 %q", got.Note)
	}
	// 未命中其它路径时的回落：wide-low 命中
	if g2, ok2 := c.Resolve("photos/b.jpg", 64); !ok2 || g2.Note != "wide-low" {
		t.Fatalf("应回落 wide-low，得到 %q ok=%v", g2.Note, ok2)
	}
}

// TestCompiled_Resolve_NegativeSizeIgnoresBounds 锁定 size<0（未知长度）只按路径匹配。
func TestCompiled_Resolve_NegativeSizeIgnoresBounds(t *testing.T) {
	t.Parallel()
	c, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `^archive/.*`, MinSize: 1, MaxSize: 20 << 20, Mode: "single", Priority: 10},
	}}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, ok := c.Resolve("archive/a.bin", -1); !ok {
		t.Fatal("size<0 应只按路径匹配（忽略体积约束）")
	}
}

// TestCompiled_RulesAndVersionCopy 锁定 Rules() 返回副本（不可改内部状态）与 Version 透传。
func TestCompiled_RulesAndVersionCopy(t *testing.T) {
	t.Parallel()
	src := []policy.Rule{{PathRegex: `^a$`, Mode: "random", Priority: 5}}
	c, err := policy.Compile(policy.Policy{Rules: src, Version: 7}, 0)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Version() != 7 {
		t.Fatalf("Version() = %d，期望 7", c.Version())
	}
	out := c.Rules()
	if len(out) != 1 {
		t.Fatalf("Rules() 长度 %d", len(out))
	}
	out[0].PathRegex = `^mutated$` // 改副本不应影响内部
	if _, ok := c.Resolve("a", 1); !ok {
		t.Fatal("改 Rules() 副本后内部规则被污染")
	}
}

// TestBlockModeConstants 锁定模式常量单一事实源（shardseal 定义，policy 引用同值）。
func TestBlockModeConstants(t *testing.T) {
	t.Parallel()
	if shardseal.BlockModeRandom != "random" || shardseal.BlockModeSingle != "single" {
		t.Fatalf("模式常量值被改动: %q / %q", shardseal.BlockModeRandom, shardseal.BlockModeSingle)
	}
}
