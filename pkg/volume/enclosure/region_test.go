// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

// region_test.go 锁定**足迹区间防误删前置校验**（`CheckFootprintRegionsFree`）：
// 新建/收养封装卷前，若声明足迹区间已存在且含「非本卷内容」（普通目录形态 / 被文件占据）
// 必须 fail-closed 拒绝——否则删除该封装卷会擦除整个区间（含既有用户数据）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// regionTestWrapper 是 region_test 私用的封装处理层名（Parse 的「wrapper 已注册」校验需要它）。
const regionTestWrapper = "region-test"

// newRegionTestSet 构造「本地 main 卷 + 一条封装声明」的卷集（真实磁盘 + 真实 storage.Root）。
// srcPath 是声明的源子路径（卷根相对，可为 owner 限定形态）。
func newRegionTestSet(t *testing.T, srcPath string) (*registry.Set, string) {
	t.Helper()
	registerTestWrapper(t, regionTestWrapper)
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: TypeEnclosure, Extra: map[string]any{"source": "main:" + srcPath, "wrapper": regionTestWrapper}},
	}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, nil, nil, "main")
	return set, dir
}

// declOf 用 Parse 解析出声明（真实解析路径：owner 限定判定/足迹推导都在其中）。
func declOf(t *testing.T, set *registry.Set) (Decl, []Decl) {
	t.Helper()
	all, err := Parse(set.All())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d, ok := declByNameHelper(all, "vault")
	if !ok {
		t.Fatal("vault 声明缺失")
	}
	return d, all
}

func declByNameHelper(all []Decl, name string) (Decl, bool) {
	for _, d := range all {
		if d.Vol == name {
			return d, true
		}
	}
	return Decl{}, false
}

// prepRegion 在基底卷根下按形态造内容（rel 为卷根相对目录）。
func prepRegion(t *testing.T, root, rel string, mk func(dir string)) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk(dir)
}

func writeFileAt(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// regionPrep 是「在基底卷根下造某形态内容」的预置函数（表驱动用例用）。
type regionPrep func(t *testing.T, root string)

// mkRegionDir 在基底卷根下建 videos/ 目录并交由 mk 填充。
func mkRegionDir(t *testing.T, root string, mk func(dir string)) {
	t.Helper()
	prepRegion(t, root, "videos", mk)
}

func prepRegionAbsent(*testing.T, string) {}

func prepEmptyRegion(t *testing.T, root string) {
	t.Helper()
	mkRegionDir(t, root, func(string) {})
}

func prepSelfBuiltRegion(t *testing.T, root string) {
	t.Helper()
	mkRegionDir(t, root, func(dir string) {
		writeFileAt(t, dir, "LAYOUT_VERSION")
		if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
}

func prepFilesOnlyRegion(t *testing.T, root string) {
	t.Helper()
	mkRegionDir(t, root, func(dir string) {
		writeFileAt(t, dir, "k1.key")
		writeFileAt(t, dir, "k2.key")
	})
}

func prepDirsOnlyRegion(t *testing.T, root string) {
	t.Helper()
	mkRegionDir(t, root, func(dir string) {
		for _, c := range []string{"aBcDe", "fGhIj"} {
			if err := os.MkdirAll(filepath.Join(dir, c), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func prepMixedRegion(t *testing.T, root string) {
	t.Helper()
	mkRegionDir(t, root, func(dir string) {
		writeFileAt(t, dir, "report.pdf")
		if err := os.MkdirAll(filepath.Join(dir, "project"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
}

func prepRegionIsFile(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "videos"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckFootprintRegionsFree_Shape 表驱动锁定内容形态判据（卷根专有声明 `main:videos`）。
func TestCheckFootprintRegionsFree_Shape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prep   regionPrep
		reject bool
	}{
		{"区间不存在", prepRegionAbsent, false},
		{"空目录", prepEmptyRegion, false},
		{"仅本卷自建结构（LAYOUT_VERSION + meta 桶）", prepSelfBuiltRegion, false},
		{"纯文件（secrets 钥匙卷形态）", prepFilesOnlyRegion, false},
		{"纯目录（secretdata 随机容器形态）", prepDirsOnlyRegion, false},
		{"文件+目录混合（普通用户目录）⇒ 拒绝", prepMixedRegion, true},
		{"区间被文件占据 ⇒ 拒绝", prepRegionIsFile, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			set, root := newRegionTestSet(t, "videos")
			c.prep(t, root)
			d, all := declOf(t, set)
			err := CheckFootprintRegionsFree(context.Background(), set, d, all)
			if c.reject {
				if !errors.Is(err, ErrRegionOccupied) {
					t.Fatalf("应拒绝（ErrRegionOccupied），got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应通过，got %v", err)
			}
		})
	}
}

// TestCheckFootprintRegionsFree_OwnerQualified 锁定**最危险形态**：owner 限定声明的足迹就是
// 该 owner 的真实数据区（`<owner>/user/<子目录>` 等保留桶），既有普通目录必须被拒。
//
// 变异（去掉 owner 限定的逐桶判定 / 只判第一条规则）⇒ 首个用例必红（用户数据面临删卷即擦除）。
func TestCheckFootprintRegionsFree_OwnerQualified(t *testing.T) {
	t.Parallel()
	t.Run("既有用户数据目录 ⇒ 拒绝", func(t *testing.T) {
		t.Parallel()
		set, root := newRegionTestSet(t, "alice/user/docs")
		prepRegion(t, root, "alice/user/docs", func(dir string) {
			writeFileAt(t, dir, "合同.txt")
			if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
				t.Fatal(err)
			}
		})
		d, all := declOf(t, set)
		if !d.OwnerQualified() || d.Owner != "alice" || d.Sub != "docs" {
			t.Fatalf("应解析为 owner 限定声明 alice/docs: %+v", d)
		}
		if err := CheckFootprintRegionsFree(context.Background(), set, d, all); !errors.Is(err, ErrRegionOccupied) {
			t.Fatalf("既有用户数据目录应被拒（ErrRegionOccupied），got %v", err)
		}
	})

	t.Run("空/本卷结构 ⇒ 通过", func(t *testing.T) {
		t.Parallel()
		set, root := newRegionTestSet(t, "alice/user/docs")
		// 「user」桶规则存在但为空；其余保留桶规则路径不存在。
		prepRegion(t, root, "alice/user/docs", func(string) {})
		d, all := declOf(t, set)
		if err := CheckFootprintRegionsFree(context.Background(), set, d, all); err != nil {
			t.Fatalf("空区间应通过，got %v", err)
		}
	})

	t.Run("整卷封装（空路径）不做区间判定 ⇒ 通过", func(t *testing.T) {
		t.Parallel()
		set, _ := newRegionTestSet(t, "")
		d, all := declOf(t, set)
		if err := CheckFootprintRegionsFree(context.Background(), set, d, all); err != nil {
			t.Fatalf("整卷封装应通过（擦除侧本就跳过卷根），got %v", err)
		}
	})
}
