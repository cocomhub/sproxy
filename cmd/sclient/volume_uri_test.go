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

	"github.com/spf13/cobra"
)

// uriTestCmd 构造带 volume/to-volume flag 的最小命令（与真实命令同 flag 名）。
func uriTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "t", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().String("volume", "", "")
	c.Flags().String(flagToVolume, "", "")
	return c
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
		{"vault:/a/b/c", "vault", "a/b/c", true, "标准形式"},
		{"vault:/", "vault", "", true, "卷根（带斜杠）"},
		{"vault:", "vault", "", true, "卷根（不带斜杠）"},
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
	if args[0] != "docs/top.txt" {
		t.Fatalf("前缀未剥离: %q", args[0])
	}
	if got, _ := cmd.Flags().GetString("volume"); got != "vault" {
		t.Fatalf("--volume=%q want vault", got)
	}
}

func TestApplyVolumeURIs_ExplicitFlagConflictFails(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	if err := cmd.Flags().Set("volume", "other"); err != nil {
		t.Fatal(err)
	}
	args := []string{"vault:/a"}
	if err := applyVolumeURIs(cmd, args); err == nil {
		t.Fatal("位置参数卷名与显式 --volume 冲突应报错（不静默取其一）")
	}
	// 同名 → 通过。
	cmd2 := uriTestCmd()
	if err := cmd2.Flags().Set("volume", "vault"); err != nil {
		t.Fatal(err)
	}
	if err := applyVolumeURIs(cmd2, []string{"vault:/a"}); err != nil {
		t.Fatalf("同名卷不应报错: %v", err)
	}
}

func TestApplyMvVolumeURIs_CrossVolumeSetsToVolume(t *testing.T) {
	t.Parallel()
	cmd := uriTestCmd()
	args := []string{"vault:/a.txt", "backup:/b.txt"}
	if err := applyMvVolumeURIs(cmd, args); err != nil {
		t.Fatalf("applyMvVolumeURIs: %v", err)
	}
	if args[0] != "a.txt" || args[1] != "b.txt" {
		t.Fatalf("前缀未剥离: %v", args)
	}
	if v, _ := cmd.Flags().GetString("volume"); v != "vault" {
		t.Fatalf("--volume=%q want vault", v)
	}
	if tv, _ := cmd.Flags().GetString(flagToVolume); tv != "backup" {
		t.Fatalf("--to-volume=%q want backup", tv)
	}

	// 同卷 → 不写 --to-volume（同卷 rename 语义）。
	cmd2 := uriTestCmd()
	if err := applyMvVolumeURIs(cmd2, []string{"vault:/a.txt", "vault:/b.txt"}); err != nil {
		t.Fatal(err)
	}
	if tv, _ := cmd2.Flags().GetString(flagToVolume); tv != "" {
		t.Fatalf("同卷不应设置 --to-volume, got %q", tv)
	}
}
