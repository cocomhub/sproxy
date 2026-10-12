// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// seal_alias_test.go —— 封印比较的**等价路径**处置（§12.1 第 3 项）：
//  1. Unicode NFC 归一（macOS APFS/HFS+ 会归一，NFC/NFD 同目录同一对象）；
//  2. Windows 8.3 短名别名展开（短名无法从长名推导，只能在装配期按磁盘实况登记）。
//
// 2026-10-12 Windows/NTFS 实测（探针，不入库）：`secret.txt.`、`secret.txt `、`dir./file`、
// 全大写路径均能打开同一文件（已由归一键覆盖）；NFC 名打不开 NFD 文件（NTFS 不归一）；
// 本机各卷未分配 8.3 短名（`dir /x` 无短名列、`%~sI` 展开等于长名）。

// TestSealCompareKey_UnicodeNFDNormalized NFC/NFD 两种写法必须归一到同一比较键
// （否则 macOS 上 `café`（NFC）封印挡不住 `cafe\u0301`（NFD）访问）。
func TestSealCompareKey_UnicodeNFDNormalized(t *testing.T) {
	t.Parallel()
	cases := [][2]string{
		{"user/vault/caf\u00e9.bin", "user/vault/cafe\u0301.bin"},
		{"user/vault/caf\u00e9.bin", "USER/VAULT/CAFE\u0301.BIN"},
		{"user/vault/A\u030angstr\u00f6m", "user/vault/\u00c5ngstro\u0308m"},
	}
	for _, c := range cases {
		if a, b := sealCompareKey(c[0]), sealCompareKey(c[1]); a != b {
			t.Fatalf("NFC/NFD 应归一为同一键:\n %q -> %q\n %q -> %q", c[0], a, c[1], b)
		}
		if !sealCovers(c[0], c[1]) {
			t.Fatalf("封印 %q 应覆盖 NFD 写法 %q", c[0], c[1])
		}
	}
	// 归一不得把不同名字混同：é 与 e 仍是两条。
	if sealCompareKey("caf\u00e9") == sealCompareKey("cafe") {
		t.Fatal("归一无权把 é 与 e 视为同一（过度归一会误封无关文件）")
	}
	// 稳定幂等：已是 NFC 的键再归一不变。
	k := sealCompareKey("user/vault/caf\u00e9")
	if sealCompareKey(k) != k {
		t.Fatalf("归一应幂等: %q -> %q", k, sealCompareKey(k))
	}
}

// TestSealCompareKey_TrailingDotSpaceAndCase 实测（Windows）可达的等价写法必须命中封印；
// 前导空格是**合法**字符（Win32 只在尾部剥离），不得被抹平。
func TestSealCompareKey_TrailingDotSpaceAndCase(t *testing.T) {
	t.Parallel()
	sealed := "user/vault"
	for _, req := range []string{
		"user/VAULT",                   // 大小写
		"user/vault.",                  // 段尾点
		"user/vault ",                  // 段尾空格
		"user/vault./secret.txt",       // 中间段尾点
		`user\vault\secret.txt`,        // 反斜杠
		"/user/vault/secret.txt",       // 前导斜杠
		"user/vault/sub/../secret.txt", // 未 Clean 的上跳（调用方另有 Abs 兜底）
	} {
		if !sealCovers(sealed, req) {
			t.Fatalf("封印 %q 应覆盖 %q", sealed, req)
		}
	}
	if sealCompareKey(" seg") == sealCompareKey("seg") {
		t.Fatal("前导空格是合法文件名成分，不得被归一抹平")
	}
}

// TestSealAliases_ExpandsShortName 8.3 短名展开：解析器给出不同短名 ⇒ 规则集必须同时含长短两种
// （短名无法从长名推导，只能装配期按实况登记）。用注入的解析器验证，不依赖本机是否启用 8.3。
func TestSealAliases_ExpandsShortName(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "user", "vault"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	longAbs := filepath.Join(dir, "user", "vault")
	old := shortPathResolver
	t.Cleanup(func() { shortPathResolver = old })
	calls := 0
	shortPathResolver = func(abs string) (string, bool) {
		calls++
		if filepath.Clean(abs) == filepath.Clean(longAbs) {
			return filepath.Join(dir, "user", "VAULT~1"), true // 模拟 8.3 短名
		}
		return "", false
	}

	if serr := rt.SetSeals([]SealRule{{Path: "user/vault", Mode: SealHidden}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	if calls == 0 {
		t.Fatal("装配期应尝试解析短名")
	}
	got := map[string]bool{}
	for _, r := range rt.Seals() {
		got[r.Path] = true
	}
	if !got["user/vault"] {
		t.Fatalf("应保留原规则, got %v", got)
	}
	if !got["user/VAULT~1"] {
		t.Fatalf("应展开出短名别名规则, got %v", got)
	}
	// 别名规则与长名同模式（写仍拒、读 404 语义一致）。
	for _, r := range rt.Seals() {
		if r.Mode != SealHidden {
			t.Fatalf("别名规则模式应继承: %+v", r)
		}
	}

	// 短名与长名**同键**（未启用 8.3 的卷上解析器可能回原路径）⇒ 不得登记重复规则（幂等）。
	shortPathResolver = func(string) (string, bool) { return longAbs, true }
	if serr := rt.SetSeals([]SealRule{{Path: "user/vault", Mode: SealHidden}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	if got := rt.Seals(); len(got) != 1 || got[0].Path != "user/vault" {
		t.Fatalf("短名同键时不得展开重复规则, got %+v", got)
	}
}

// TestSealAliases_NoShortNameNoExpansion 解析器不掌握（非 Windows / 未启用 8.3）⇒ 零回归：
// 规则集与输入一致。
func TestSealAliases_NoShortNameNoExpansion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if serr := rt.SetSeals([]SealRule{{Path: "user/vault", Mode: SealReadable}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	if got := rt.Seals(); len(got) != 1 || got[0].Path != "user/vault" {
		t.Fatalf("无短名时不应展开, got %+v", got)
	}
}

// TestResolveShortPath_PlatformDefault 平台解析器在**默认环境**下不得 panic，且给出的短名（若有）
// 必须与长名指向同一路径（本机未启用 8.3 时恒返回 ok=false）。
func TestResolveShortPath_PlatformDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	longDir := filepath.Join(dir, "verylongdirectoryname-for-shortname-probe-1234")
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	short, ok := resolveShortPath(longDir)
	if !ok {
		return // 本机未启用 8.3：正常（实测即此）
	}
	if sealCompareKey(short) == sealCompareKey(longDir) {
		t.Fatalf("报告存在短名却与长名同键: %q vs %q", short, longDir)
	}
	if _, err := os.Stat(short); err != nil {
		t.Fatalf("短名 %q 应指向真实路径: %v", short, err)
	}
}
