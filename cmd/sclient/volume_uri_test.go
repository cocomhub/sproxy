// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// volume_uri_test.go 钉住卷 URI 寻址的解析与 flag 归一（用户要求 2026-10-13：
// 「secretdata 卷 A → A:/a/b/c」）：
//   - `<卷名>:/<路径>` / `<卷名>:` / `<卷名>:/` = 卷根；
//   - URL（`https://…`）与 Windows 盘符（`C:\x`）**不得**被误判为卷 URI；
//   - 位置参数与显式 `--volume` 冲突 → 报错（不静默取其一）；
//   - mv：源卷写 --volume、目标不同卷写 --to-volume、同卷不写。
//
// 变异：把 `strings.HasPrefix(rest, "//")` 的 URL 排除去掉 → URL 用例红；
// 去掉冲突报错 → 冲突用例红；mv 目标卷不写 --to-volume → 跨卷用例红。

import (
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/volumectx"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/spf13/cobra"
)

// uriTestCmd 构造最小命令（卷经 **上下文** 传递——用户口径 2026-10-14：卷寻址只有路径内 URI，
// 无 --volume flag）。
func uriTestCmd() *cobra.Command {
	return &cobra.Command{Use: "t", RunE: func(*cobra.Command, []string) error { return nil }}
}

func TestSplitVolumeURI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg     string
		vol     string
		path    string
		ok      bool
		comment string
	}{
		{"vault:/a/b/c", "vault", "/a/b/c", true, "标准形式（卷根绝对）"},
		{"vault:/", "vault", "/", true, "卷根（带斜杠）"},
		{"vault:", "vault", "/", true, "卷根（不带斜杠）"},
		{"vault:dir/f.txt", "", "vault:dir/f.txt", false, "缺 `:/`（不得误判）"},
		{"https://example.com/a", "", "https://example.com/a", false, "URL 不得误判"},
		{"C:\\Users\\x", "", "C:\\Users\\x", false, "Windows 盘符不得误判"},
		{"plain.txt", "", "plain.txt", false, "无前缀"},
		{":/a", "", ":/a", false, "空卷名"},
		{"bad name:/a", "", "bad name:/a", false, "卷名含空格"},
	}
	for _, c := range cases {
		vol, path, ok := splitVolumeURI(c.arg)
		if ok != c.ok || vol != c.vol || path != c.path {
			t.Errorf("splitVolumeURI(%q) = (%q,%q,%v), want (%q,%q,%v) — %s",
				c.arg, vol, path, ok, c.vol, c.path, c.ok, c.comment)
		}
	}
}

func TestApplyVolumeURIs_SetsFlagAndStripsPrefix(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	args := []string{"vault:/docs/top.txt"}
	if err := applyVolumeURIs(cmd, args); err != nil {
		t.Fatalf("applyVolumeURIs: %v", err)
	}
	if args[0] != "/docs/top.txt" {
		t.Fatalf("URI 路径应为卷根绝对（保留前导斜杠）: %q", args[0])
	}
	if got, _ := volumectx.From(cmd.Context()); got != "vault" {
		t.Fatalf("--volume=%q want vault", got)
	}
}

func TestApplyVolumeURIs_MultiArgConflictFails(t *testing.T) {
	t.Parallel()
	// 单卷命令的多个位置参数指向**不同卷** ⇒ 报错（不静默取其一）。
	cmd := uriTestCmd()
	args := []string{"vault:/a", "other:/b"}
	if err := applyVolumeURIs(cmd, args); err == nil {
		t.Fatal("多个位置参数卷名冲突应报错（不静默取其一）")
	}
	// 同名 → 通过，且前缀被剥离。
	cmd2 := uriTestCmd()
	args2 := []string{"vault:/a", "vault:/b"}
	if err := applyVolumeURIs(cmd2, args2); err != nil {
		t.Fatalf("同名卷不应报错: %v", err)
	}
	if args2[0] != "/a" || args2[1] != "/b" {
		t.Fatalf("URI 前缀应被剥离: %v", args2)
	}
	if got, _ := volumectx.From(cmd2.Context()); got != "vault" {
		t.Fatalf("源卷上下文=%q want vault", got)
	}
}

func TestApplyMvVolumeURIs_CrossVolumeSetsToVolume(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	args := []string{"vault:/a.txt", "backup:/b.txt"}
	if err := applyMvVolumeURIs(cmd, args); err != nil {
		t.Fatalf("applyMvVolumeURIs: %v", err)
	}
	if args[0] != "/a.txt" || args[1] != "/b.txt" {
		t.Fatalf("URI 路径应为卷根绝对: %v", args)
	}
	if v, _ := volumectx.From(cmd.Context()); v != "vault" {
		t.Fatalf("源卷上下文=%q want vault", v)
	}
	if tv, _ := volumectx.Target(cmd.Context()); tv != "backup" {
		t.Fatalf("目标卷上下文=%q want backup", tv)
	}

	// 同卷 → 不写 --to-volume（同卷 rename 语义）。
	cmd2 := uriTestCmd()
	if err := applyMvVolumeURIs(cmd2, []string{"vault:/a.txt", "vault:/b.txt"}); err != nil {
		t.Fatal(err)
	}
	if tv, ok := volumectx.Target(cmd2.Context()); ok {
		t.Fatalf("同卷不应设置目标卷上下文, got %q", tv)
	}
}

// ---- 工作目录所在卷回落（用户口径 2026-10-13）----

func TestApplyVolumeContext_StateVolumeFillsContext(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	st := &state.State{CurrentVolume: "vault", CurrentDir: "docs"}
	if err := applyVolumeContext(cmd, st, nil); err != nil {
		t.Fatalf("applyVolumeContext: %v", err)
	}
	if got, _ := volumectx.From(cmd.Context()); got != "vault" {
		t.Fatalf("--volume=%q want vault（当前工作目录所在卷）", got)
	}
}

func TestApplyVolumeContext_Precedence(t *testing.T) {
	t.Parallel()
	st := &state.State{CurrentVolume: "cwd-vol"}
	// 工作目录卷（cd 设定）在**无 URI** 时生效。
	cmd := uriTestCmd()
	if err := applyVolumeContext(cmd, st, nil); err != nil {
		t.Fatalf("applyVolumeContext: %v", err)
	}
	if got, _ := volumectx.From(cmd.Context()); got != "cwd-vol" {
		t.Fatalf("无 URI 时应取工作目录卷, got %q", got)
	}
	// 位置参数卷 URI 优先于工作目录卷（且位置参数会剥离前缀）。
	cmd2 := uriTestCmd()
	args := []string{"uri-vol:/a.txt"}
	if err := applyVolumeContext(cmd2, st, args); err != nil {
		t.Fatalf("applyVolumeContext: %v", err)
	}
	if got, _ := volumectx.From(cmd2.Context()); got != "uri-vol" {
		t.Fatalf("URI 应优先于工作目录卷, got %q", got)
	}
	if args[0] != "/a.txt" {
		t.Fatalf("URI 路径应为卷根绝对（保留前导斜杠）: %q", args[0])
	}
}

func TestApplyMvVolumeContext_StateVolumeAppliesToBothSides(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	st := &state.State{CurrentVolume: "vault", CurrentDir: "docs"}
	if err := applyMvVolumeContext(cmd, st, []string{"a.txt", "b.txt"}); err != nil {
		t.Fatalf("applyMvVolumeContext: %v", err)
	}
	if got, _ := volumectx.From(cmd.Context()); got != "vault" {
		t.Fatalf("源卷上下文=%q want vault", got)
	}
	// 目标显式带不同卷 ⇒ 跨卷迁移（目标卷上下文），源仍取 URI/工作目录卷。
	cmd2 := uriTestCmd()
	if err := applyMvVolumeContext(cmd2, st, []string{"a.txt", "other:/b.txt"}); err != nil {
		t.Fatalf("applyMvVolumeContext: %v", err)
	}
	if v, _ := volumectx.From(cmd2.Context()); v != "vault" {
		t.Fatalf("源卷应取工作目录卷, got %q", v)
	}
	if tv, _ := volumectx.Target(cmd2.Context()); tv != "other" {
		t.Fatalf("--to-volume=%q want other", tv)
	}
}
