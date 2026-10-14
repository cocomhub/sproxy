// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newSealRoot 构造一个带 fixture 的 storage.Root：
//
//	private/vault/secret.bin（将被封印的封装区）
//	private/plain.bin      （同目录的普通文件，用于验证不误伤）
func newSealRoot(t *testing.T, rules ...SealRule) *Root {
	t.Helper()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := os.MkdirAll(filepath.Join(dir, "private", "vault"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "vault", "secret.bin"), []byte("cipher"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private", "plain.bin"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(rules) > 0 {
		if err := root.SetSeals(rules); err != nil {
			t.Fatalf("SetSeals: %v", err)
		}
	}
	return root
}

// TestRoot_SetSeals_Validation 封印路径必须归一/合法（fail-closed 装配期拒绝）。
func TestRoot_SetSeals_Validation(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t)
	bad := [][]SealRule{
		// 注意：**空字符串**是合法的「整卷封印」（见 TestRoot_SetSeals_WholeVolume），
		// 但**纯空白**是拼写错误（非显式整卷语义）⇒ 仍拒绝（fail-closed）。
		{{Path: "   ", Wrapper: "w"}},
		{{Path: "/abs/dir", Wrapper: "w"}},
		{{Path: "../escape", Wrapper: "w"}},
		{{Path: "a/../../b", Wrapper: "w"}},
		{{Path: "C:/win", Wrapper: "w"}},
	}
	for i, rules := range bad {
		if err := root.SetSeals(rules); err == nil {
			t.Errorf("case %d: 非法封印路径应报错: %+v", i, rules)
		}
	}
	// 归一：前后的斜杠/反斜杠/空白均被接受并归一（前导斜杠=绝对路径，必须拒绝，见上）。
	if err := root.SetSeals([]SealRule{{Path: ` private\vault/ `, Wrapper: "vault"}}); err != nil {
		t.Fatalf("归一化路径应被接受: %v", err)
	}
	if got := root.Seals(); len(got) != 1 || got[0].Path != "private/vault" {
		t.Fatalf("封印路径应归一为 private/vault, got %+v", got)
	}
	// 清空（零回归）。
	if err := root.SetSeals(nil); err != nil {
		t.Fatal(err)
	}
	if got := root.Seals(); len(got) != 0 {
		t.Fatalf("SetSeals(nil) 应清空, got %+v", got)
	}
}

// TestRoot_SealHidden_ReadsAre404_WritesAlwaysRejected 默认模式：读=不存在（且可归因），
// 写=一律拒绝（无配置开关能放开写）。
func TestRoot_SealHidden_ReadsAre404_WritesAlwaysRejected(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "private/vault", Wrapper: "vault"})

	// 读侧：所有读入口 → fs.ErrNotExist（404）+ 可归因 ErrSealed。
	reads := map[string]func() error{
		"Open": func() error {
			f, err := root.Open("private/vault/secret.bin")
			if err == nil {
				_ = f.Close()
			}
			return err
		},
		"OpenDecrypted": func() error {
			rc, err := root.OpenDecrypted("private/vault/secret.bin")
			if err == nil {
				_ = rc.Close()
			}
			return err
		},
		"Stat":        func() error { _, err := root.Stat("private/vault/secret.bin"); return err },
		"Lstat":       func() error { _, err := root.Lstat("private/vault"); return err },
		"LogicalSize": func() error { _, err := root.LogicalSize("private/vault/secret.bin"); return err },
		"ReadDir(自身)": func() error { _, err := root.ReadDir("private/vault"); return err },
	}
	for name, fn := range reads {
		err := fn()
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: 默认模式下读封印路径应按不存在处理, got %v", name, err)
		}
		if !errors.Is(err, ErrSealed) {
			t.Errorf("%s: 读错误应可归因 ErrSealed, got %v", name, err)
		}
	}

	// 写侧：一切写入口 → ErrSealed（且不可被误认为「不存在」）。
	writes := map[string]func() error{
		"OpenFile(O_WRONLY)": func() error {
			f, err := root.OpenFile("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				_ = f.Close()
			}
			return err
		},
		"OpenFileEncrypted": func() error {
			w, err := root.OpenFileEncrypted("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				_ = w.Close()
			}
			return err
		},
		"MkdirAll": func() error { return root.MkdirAll("private/vault/sub", 0o755) },
		"Remove":   func() error { return root.Remove("private/vault/secret.bin") },
		"RemoveAll": func() error {
			return root.RemoveAll("private/vault")
		},
		"Rename(移入)": func() error { return root.Rename("private/plain.bin", "private/vault/in.bin") },
		"Rename(移出)": func() error { return root.Rename("private/vault/secret.bin", "private/out.bin") },
		"Link(入)":    func() error { return root.Link("private/plain.bin", "private/vault/l.bin") },
		"Link(出)":    func() error { return root.Link("private/vault/secret.bin", "private/l.bin") },
		"Chtimes":    func() error { return root.Chtimes("private/vault/secret.bin", time.Now(), time.Now()) },
	}
	for name, fn := range writes {
		err := fn()
		if !errors.Is(err, ErrSealed) {
			t.Errorf("%s: 写封印路径必须被拒绝(ErrSealed), got %v", name, err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: 写拒绝错误不应表现为『不存在』(否则上层会误判为 404 而非 409)", name)
		}
	}

	// 未封印的兄弟路径不受影响（零误伤）。
	if _, err := root.Stat("private/plain.bin"); err != nil {
		t.Errorf("同目录未封印文件应可读: %v", err)
	}
	if err := root.MkdirAll("private/other", 0o755); err != nil {
		t.Errorf("同目录未封印路径应可写: %v", err)
	}
}

// TestRoot_SealHidden_ParentListingFiltersEntry 封印条目不得出现在父目录列举中
// （否则泄漏封装布局的存在与名字）。
func TestRoot_SealHidden_ParentListingFiltersEntry(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "private/vault", Wrapper: "vault"})
	entries, err := root.ReadDir("private")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "vault" {
			t.Fatalf("默认模式下列举父目录不应出现封印条目 vault: %+v", entries)
		}
	}
	if len(entries) != 1 || entries[0].Name() != "plain.bin" {
		t.Fatalf("父目录列举应只剩 plain.bin, got %+v", entries)
	}
}

// TestRoot_SealReadable_ReadAllowedWriteStillDenied 只读透视模式（卷迁移/查看原始存储形态）：
// 读放行且可列举，写**仍然绝对禁止**。
func TestRoot_SealReadable_ReadAllowedWriteStillDenied(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "private/vault", Wrapper: "vault", Mode: SealReadable})

	rc, err := root.Open("private/vault/secret.bin")
	if err != nil {
		t.Fatalf("只读透视模式读应放行: %v", err)
	}
	buf := make([]byte, 16)
	n, _ := rc.Read(buf)
	_ = rc.Close()
	if got := string(buf[:n]); got != "cipher" {
		t.Fatalf("应读到原始存储字节 cipher, got %q", got)
	}
	if _, serr := root.Stat("private/vault/secret.bin"); serr != nil {
		t.Fatalf("只读透视模式 Stat 应放行: %v", serr)
	}
	entries, err := root.ReadDir("private")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "vault" {
			found = true
		}
	}
	if !found {
		t.Fatalf("只读透视模式下列举应保留封印条目, got %+v", entries)
	}

	// 写：任何模式都不放行。
	if rerr := root.Remove("private/vault/secret.bin"); !errors.Is(rerr, ErrSealed) {
		t.Errorf("只读透视模式下删除仍必须拒绝, got %v", rerr)
	}
	f, err := root.OpenFile("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrSealed) {
		t.Errorf("只读透视模式下写仍必须拒绝, got %v", err)
	}
}

// TestRoot_Seal_NestedDeeperRuleWins 嵌套封印：更深的规则优先给出更精确的 wrapper 归因。
func TestRoot_Seal_NestedDeeperRuleWins(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "private", Wrapper: "outer"}, SealRule{Path: "private/vault/inner", Wrapper: "inner"})
	if r := root.SealAt("private/vault/inner/deep/f.bin"); r == nil || r.Wrapper != "inner" {
		t.Fatalf("更深封印规则应优先, got %+v", r)
	}
	if r := root.SealAt("private/vault/other.bin"); r == nil || r.Wrapper != "outer" {
		t.Fatalf("外层封印应覆盖未被内层覆盖的路径, got %+v", r)
	}
	if r := root.SealAt("public/f.bin"); r != nil {
		t.Fatalf("未封印路径应为 nil, got %+v", r)
	}
}

// TestRoot_NoSeals_ZeroRegression 未配置封印时读写行为与既有完全一致。
func TestRoot_NoSeals_ZeroRegression(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t)
	if err := root.MkdirAll("private/vault", 0o755); err != nil {
		t.Fatalf("无封印时目录可建: %v", err)
	}
	f, err := root.OpenFile("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("无封印时可写: %v", err)
	}
	_ = f.Close()
	if rerr := root.Remove("private/vault/x.bin"); rerr != nil {
		t.Fatalf("无封印时可删: %v", rerr)
	}
	entries, err := root.ReadDir("private")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("无封印时列举应包含全部条目, got %+v", entries)
	}
}

// TestRoot_Seal_CaseInsensitiveAndTrailingDot 评审 P0：封印前缀比较必须是**比较归一键**
// （大小写不敏感 + 逐段去尾点/空格）——否则在大小写不敏感文件系统（Windows NTFS / macOS APFS
// 默认）上 `user/PRIVATE/vault`、`user/vault./x`、`user/vault /x` 都能绕过封印直连源存储。
// 变异：sealCovers 改回字节前缀比较 → 三个变体用例全红。
func TestRoot_Seal_CaseInsensitiveAndTrailingDot(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "private/vault", Wrapper: "vault"})

	variants := []string{
		"private/VAULT/x.bin",          // 大小写变体
		"PRIVATE/vault/x.bin",          // 目录段大小写
		"private/vault./x.bin",         // Windows 会剥离段尾点 → 实际命中同一目录
		"private/vault /x.bin",         // 段尾空格同理
		"private/vault/../vault/x.bin", // 经 `..` 绕一圈仍落在封印目录
	}
	for _, rel := range variants {
		if err := root.Remove(rel); !errors.Is(err, ErrSealed) {
			t.Errorf("变体 %q 的写必须被拒(ErrSealed), got %v", rel, err)
		}
		if _, err := root.Stat(rel); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("变体 %q 的读应按不存在, got %v", rel, err)
		}
	}
	// 未封印路径不受影响（大小写敏感 FS 上的「多封」是保守方向，但不得影响无关路径）。
	if err := root.MkdirAll("public/ok", 0o755); err != nil {
		t.Fatalf("无关路径应可用: %v", err)
	}
}

// TestRoot_SetSeals_WholeVolume 空路径 = **整卷封印**：设计 2026-10-11-enclosure-volume §4/§5
// 与 docs/config.md 明确「`extra.source` 子路径可空 = 整卷封装」，`enclosure.KeySpacePrefix()`
// 也按空前缀实现；但 storage 此前把空路径判非法（"封印路径不能为空"）⇒ 该配置**装配期恒失败**
// （文档承诺与实现矛盾，两边各自有测试故长期未被发现）。本用例钉住「整卷封印」语义：
// 卷内**任意**路径写恒拒、读按 hidden 404（readable 可读仍禁写）、根列举被过滤。
//
// 变异：`normalizeSealPath` 重新拒绝空路径 → 装配期报错/本用例红；`sealCovers` 不特判空串
// → 空规则匹配不到任何路径（形同虚设）→ 本用例红。
func TestRoot_SetSeals_WholeVolume(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "", Wrapper: "vault"})
	if got := root.Seals(); len(got) != 1 || got[0].Path != "" || got[0].Wrapper != "vault" {
		t.Fatalf("整卷封印规则应被接受, got %+v", got)
	}
	// 写：卷内任意路径（含深层与既有文件）恒拒且可归因。
	for _, rel := range []string{"private/plain.bin", "private/vault/secret.bin", "brand/new/deep.bin"} {
		w, werr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY, 0o644)
		if werr == nil {
			_ = w.Close()
			t.Fatalf("整卷封印下写 %s 必须被拒绝", rel)
		}
		if !errors.Is(werr, ErrSealed) {
			t.Fatalf("写 %s 拒绝应可归因 ErrSealed, got %v", rel, werr)
		}
	}
	// 读：默认 hidden → 按不存在（同时可归因 ErrSealed）。
	if _, rerr := root.Stat("private/vault/secret.bin"); !errors.Is(rerr, fs.ErrNotExist) || !errors.Is(rerr, ErrSealed) {
		t.Fatalf("整卷封印读应既不存在又可归因, got %v", rerr)
	}
	// 列举：整卷规则覆盖**任意**目录（含其自身）⇒ 父目录列举按 hidden 视为不存在。
	if _, lerr := root.ReadDir("private"); !errors.Is(lerr, fs.ErrNotExist) || !errors.Is(lerr, ErrSealed) {
		t.Fatalf("整卷 hidden 封印下目录列举应按不存在, got %v", lerr)
	}
	// 规则覆盖面：任意路径（含仅一层、深层、以及 root 自身以外的所有形态）。
	for _, rel := range []string{"private", "private/vault", "user/a.bin", "meta/x.meta"} {
		if rule := root.SealAt(rel); rule == nil || rule.Wrapper != "vault" {
			t.Fatalf("整卷封印应覆盖 %q, got %+v", rel, rule)
		}
	}
}

// TestRoot_SetSeals_WholeVolumeReadable 整卷封印 + readable：只读透视（可读原始形态），
// 写仍绝对禁止。迁移场景需要（整卷带外核对）。
func TestRoot_SetSeals_WholeVolumeReadable(t *testing.T) {
	t.Parallel()
	root := newSealRoot(t, SealRule{Path: "", Wrapper: "vault", Mode: SealReadable})
	rc, err := root.OpenDecrypted("private/vault/secret.bin")
	if err != nil {
		t.Fatalf("readable 整卷封印应可读: %v", err)
	}
	_ = rc.Close()
	if _, werr := root.OpenFile("private/plain.bin", os.O_CREATE|os.O_WRONLY, 0o644); !errors.Is(werr, ErrSealed) {
		t.Fatalf("readable 下写仍须拒绝, got %v", werr)
	}
	// 列举可见（readable 不隐藏）。
	ents, lerr := root.ReadDir("private")
	if lerr != nil || len(ents) == 0 {
		t.Fatalf("readable 整卷封印应保留列举可见性, got n=%d err=%v", len(ents), lerr)
	}
}
