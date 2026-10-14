// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// sealfs_test.go 钉住 **FS 级封印装饰器**（外部卷视图的封印）：
//   - 写面全拒（含改名两端、移动/复制/链接两端、staging、容量预检、meta 旁路）——无配置开关；
//   - 读面 hidden ⇒ 同时可归因 fs.ErrNotExist（404）与 storage.ErrSealed；readable ⇒ 放行；
//   - 列举过滤（hidden）/保留（readable）；
//   - 直链抑制（直链 = 客户端直连源存储 = 绕过封印）；
//   - Inner() 特权解包（封装卷自身仍能读写被封区间——与本地源的特权句柄同型）；
//   - 空路径规则 = 整卷封印。
//
// 变异：去掉写面 guard → 写断言红；把 readErr 改成恒 nil → 读 404 断言红；
// 去掉 sealFilter → 列举过滤断言红；去掉 DirectURL 抑制 → 直链断言红。

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newSealFSFixture 构造「本地 FS + 封印 private/vault」视图（hidden 模式）。
func newSealFSFixture(t *testing.T) (*SealFS, syncpkg.FS) {
	t.Helper()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	for _, p := range []string{"private/vault", "private/other", "user"} {
		if err := raw.MakeDir(context.Background(), p); err != nil {
			t.Fatalf("MakeDir(%s): %v", p, err)
		}
	}
	rules := []storage.SealRule{{Path: "private/vault", Wrapper: "vault", Mode: storage.SealHidden}}
	return NewSealFS(raw, rules), raw
}

// assertSealedWrite 断言写面错误可归因 ErrSealed。
func assertSealedWrite(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：封印路径写操作必须被拒（ErrSealed）", what)
	}
	if !errors.Is(err, storage.ErrSealed) {
		t.Fatalf("%s：错误应可归因 storage.ErrSealed, got %v", what, err)
	}
}

// assertSealedRead 断言读面返回「不存在」且可归因 ErrSealed（404 + 可观测）。
func assertSealedRead(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：封印路径（hidden）读操作必须表现为不存在", what)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s：读错误应匹配 fs.ErrNotExist（404 语义）, got %v", what, err)
	}
	if !errors.Is(err, storage.ErrSealed) {
		t.Fatalf("%s：读错误应可归因 storage.ErrSealed（可观测）, got %v", what, err)
	}
}

func TestSealFS_WriteOperationsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sf, raw := newSealFSFixture(t)
	sealed := "private/vault/f.bin"

	assertSealedWrite(t, sf.WriteFile(ctx, sealed, strings.NewReader("x"), 1, 0), "WriteFile")
	assertSealedWrite(t, sf.MakeDir(ctx, "private/vault/sub"), "MakeDir")
	assertSealedWrite(t, sf.Delete(ctx, sealed), "Delete")
	assertSealedWrite(t, sf.Rename(ctx, sealed, "user/moved.bin"), "Rename 移出封印区")
	assertSealedWrite(t, sf.Rename(ctx, "user/keep.bin", sealed), "Rename 移入封印区")
	assertSealedWrite(t, sf.WriteFromStagedPath(ctx, sealed, "/tmp/staged", 1, 0), "WriteFromStagedPath")
	assertSealedWrite(t, sf.ReserveSpace(ctx, sealed, 10), "ReserveSpace")
	if _, werr := sf.WriteIfAbsent(ctx, sealed, strings.NewReader("x"), 1, 0); !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("WriteIfAbsent 应被拒（不得返回 false,nil——会被读成「已存在」）, got %v", werr)
	}
	// 非封印路径不受影响（零回归）。
	if werr := sf.WriteFile(ctx, "private/other/ok.bin", strings.NewReader("ok"), 2, 0); werr != nil {
		t.Fatalf("非封印路径写应放行, got %v", werr)
	}
	if _, serr := raw.Stat(ctx, "private/other/ok.bin"); serr != nil {
		t.Fatalf("非封印路径内容应真的写入底层: %v", serr)
	}
}

func TestSealFS_ReadOperationsHidden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sf, raw := newSealFSFixture(t)
	// 底层真实存在（证明「不可读」是封印所致，而非本来就没有）。
	if err := raw.WriteFile(ctx, "private/vault/f.bin", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatal(err)
	}
	assertSealedRead(t, func() error { _, err := sf.OpenRead(ctx, "private/vault/f.bin"); return err }(), "OpenRead")
	assertSealedRead(t, func() error { _, err := sf.Stat(ctx, "private/vault/f.bin"); return err }(), "Stat")
	assertSealedRead(t, func() error {
		_, err := sf.OpenRangeRead(ctx, "private/vault/f.bin", 0, 1)
		return err
	}(), "OpenRangeRead")
	assertSealedRead(t, func() error { _, err := sf.FileMeta(ctx, "private/vault/f.bin"); return err }(), "FileMeta")
	// 目录前缀也封印（子树整体不可达）。
	assertSealedRead(t, func() error { _, err := sf.ListDir(ctx, "private/vault"); return err }(), "ListDir(封印目录)")
	// 父目录可列举，且**过滤**掉封印条目（不泄漏布局）。
	entries, lerr := sf.ListDir(ctx, "private")
	if lerr != nil {
		t.Fatalf("父目录列举应可用: %v", lerr)
	}
	for _, e := range entries {
		if e.Name == "vault" {
			t.Fatalf("hidden 模式下父目录列举必须过滤封印条目, got %+v", entries)
		}
	}
	if len(entries) == 0 {
		t.Fatal("父目录应仍有其它条目（判据非空自证）")
	}
}

func TestSealFS_ReadableModeAllowsReadsRejectsWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	if err := raw.WriteFile(ctx, "private/vault/f.bin", strings.NewReader("payload"), 7, 0); err != nil {
		t.Fatal(err)
	}
	sf := NewSealFS(raw, []storage.SealRule{{Path: "private/vault", Wrapper: "vault", Mode: storage.SealReadable}})
	// 读放行（只读透视）。
	rc, err := sf.OpenRead(ctx, "private/vault/f.bin")
	if err != nil {
		t.Fatalf("readable 模式读应放行: %v", err)
	}
	_ = rc.Close()
	// 写仍绝对禁止。
	assertSealedWrite(t, sf.WriteFile(ctx, "private/vault/f.bin", strings.NewReader("x"), 1, 0), "readable 写")
}

func TestSealFS_InnerIsPrivileged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sf, _ := newSealFSFixture(t)
	// 特权视图 = 最内层 FS（封装卷自身读写被封区间不受封印影响）。
	inner := syncpkg.Innermost(sf)
	if inner == nil {
		t.Fatal("Innermost 应能解包封印装饰器")
	}
	if werr := inner.WriteFile(ctx, "private/vault/f.bin", bytes.NewReader([]byte("plain")), 5, 0); werr != nil {
		t.Fatalf("特权视图必须能写入被封区间（封装卷自身）, got %v", werr)
	}
	// 而封印视图仍拒绝直连读（同一底层）。
	assertSealedRead(t, func() error { _, err := sf.OpenRead(ctx, "private/vault/f.bin"); return err }(), "OpenRead(封印视图)")
}

func TestSealFS_DirectURLSuppressedForSealedPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := &directURLFS{}
	sf := NewSealFS(inner, []storage.SealRule{{Path: "private/vault", Wrapper: "vault"}})
	// 命中封印 ⇒ 抑制（直链 = 客户端直连源存储，绕过封印）。
	if u, ok, err := sf.DirectURL(ctx, "private/vault/f.bin"); ok || err != nil || u != "" {
		t.Fatalf("封印路径直链必须抑制, got url=%q ok=%v err=%v", u, ok, err)
	}
	// 未命中 ⇒ 透传。
	if u, ok, err := sf.DirectURL(ctx, "private/other/f.bin"); !ok || err != nil || u == "" {
		t.Fatalf("非封印路径直链应透传, got url=%q ok=%v err=%v", u, ok, err)
	}
}

func TestSealFS_WholeVolumeRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	sf := NewSealFS(raw, []storage.SealRule{{Path: "", Wrapper: "vault"}}) // 空路径 = 整卷封印
	assertSealedWrite(t, sf.WriteFile(ctx, "any/path.bin", strings.NewReader("x"), 1, 0), "整卷封印写")
	assertSealedRead(t, func() error { _, err := sf.OpenRead(ctx, "any/path.bin"); return err }(), "整卷封印读")
}

// directURLFS 是最小直链提供者替身（钉住封印视图的直链抑制/透传）。
type directURLFS struct {
	syncpkg.FS
}

func (d *directURLFS) DirectURL(_ context.Context, relPath string) (string, bool, error) {
	return "https://cdn.example/" + relPath, true, nil
}
