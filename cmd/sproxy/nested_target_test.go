// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// nested_target_test.go 验证嵌套封装的底层解析（resolveTargetFS / resolveNestedTargetFS）：
//   - 嵌套 target `<卷>/<新子目录>`：MakeDir 创建空目录 + 可写 SubFS 封装（写落子目录内）；
//   - 子目录已存在 → fail-closed 报错；
//   - 历史路径 `target: local + extra.root` 零回归（本地根整卷，不进嵌套语义）。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// storeTempSet 把测试 set Store 进全局 secretDataSet（resolveTargetFS 读它），并归还旧值。
func storeTempSet(t *testing.T) {
	t.Helper()
	prev := secretDataSet.Load()
	set := newTestSet(t)
	secretDataSet.Store(set)
	t.Cleanup(func() { secretDataSet.Store(prev) })
	_ = set
}

// TestResolveTargetFS_Nested_CreatesSubdirAndConfines 嵌套 target 创建空子目录并限制写落其内。
func TestResolveTargetFS_Nested_CreatesSubdirAndConfines(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例（resolveTarget 读取），不并发。
	storeTempSet(t)
	ctx := context.Background()
	root := secretDataSet.Load().Default().RootDir

	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/videos"}}
	fs, err := resolveTargetFS(ctx, v)
	if err != nil {
		t.Fatalf("resolveTargetFS(default/videos): %v", err)
	}
	// 子目录已 MakeDir
	if _, err := os.Stat(filepath.Join(root, "videos")); err != nil {
		t.Fatalf("MakeDir 后 default/videos 应存在: %v", err)
	}
	// 写落在 <root>/videos/ 下（不污染根）
	if err := fs.WriteFile(ctx, "dirA/x.txt", strings.NewReader("hi"), 2, 0); err != nil {
		t.Fatalf("嵌套写入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "videos", "dirA", "x.txt")); err != nil {
		t.Fatalf("密文未落 <root>/videos/dirA/x.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "dirA")); !os.IsNotExist(err) {
		t.Fatalf("写逃出子目录（根下不应有 dirA）")
	}
	// 读可还原（ListDir Path 相对嵌套根）
	if e, _ := fs.Stat(ctx, "dirA/x.txt"); e == nil {
		t.Fatal("Stat dirA/x.txt 应可查")
	}
}

// TestResolveNestedTargetFS_SubdirExists_FailClosed 子目录已存在（防与底层卷既有数据混合）→ 报错。
func TestResolveNestedTargetFS_SubdirExists_FailClosed(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并发。
	storeTempSet(t)
	ctx := context.Background()
	root := secretDataSet.Load().Default().RootDir
	if err := os.MkdirAll(filepath.Join(root, "videos"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/videos"}}
	if _, err := resolveTargetFS(ctx, v); err == nil {
		t.Fatal("子目录已存在应 fail-closed 报错")
	}
}

// TestResolveTargetFS_LegacyLocal_ZeroRegression 历史路径 target=local + extra.root：
// 返回本地根整卷 LocalFS（不进互斥占用/子目录语义），写落在 root 根。
func TestResolveTargetFS_LegacyLocal_ZeroRegression(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "local", "root": root}}
	fs, err := resolveTargetFS(ctx, v)
	if err != nil {
		t.Fatalf("resolveTargetFS(local+root): %v", err)
	}
	if err := fs.WriteFile(ctx, "a.txt", strings.NewReader("hi"), 2, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 写直接落在 root 根（无子目录）
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("local+root 应把文件写 root 根: %v", err)
	}
}

// TestSplitNestedTarget 解析边界。
func TestSplitNestedTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in             string
		wantBase, want string
		wantOK         bool
	}{
		{in: "main/videos", wantBase: "main", want: "videos", wantOK: true},
		{in: "main/a/b", wantBase: "main", want: "a/b", wantOK: true},
		{in: "main", wantOK: false},
		{in: "", wantOK: false},
		{in: "main/", wantOK: false},
		{in: "/sub", wantOK: false},
	}
	for _, c := range cases {
		b, s, ok := volume.SplitNestedTarget(c.in)
		if ok != c.wantOK || b != c.wantBase || s != c.want {
			t.Errorf("SplitNestedTarget(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, b, s, ok, c.wantBase, c.want, c.wantOK)
		}
	}
}

// TestNestedSecretdata_EndToEnd 嵌套封装全链路：target=default/videos → resolveTargetFS
// MakeDir + SubFS 可写底层 → secretdata 经其写密文（落 <root>/videos/）→ 读还原明文。
// 验证「可写封装子视图」是嵌套封装的正确底层（只读视图会写失败——这是设计与实现的关键差异）。
func TestNestedSecretdata_EndToEnd(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例（resolveTargetFS 读取），不并发。
	ctx := context.Background()
	prev := secretDataSet.Load()
	set := newTestSet(t)
	secretDataSet.Store(set)
	t.Cleanup(func() { secretDataSet.Store(prev) })

	// 默认 secrets 卷 + 造密钥（复用 ensureDefaultSecretsVolume）。
	if _, err := ensureDefaultSecretsVolume(ctx, set, set.Default().RootDir, nil); err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	registerSecretsBackend()
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}

	typ := "secretdata-nested"
	registerSecretdataBackendWithFS(typ, func(c context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(c, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })

	root := set.Default().RootDir
	v := volume.Volume{Name: "sd", Type: typ, Extra: map[string]any{
		"target":     "default/videos",
		"secret_url": "secrets://default/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend(嵌套 secretdata): %v", err)
	}
	// 透明写/读
	if werr := be.FS().WriteFile(ctx, "movie.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("嵌套卷 WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "movie.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q, want hello secret", buf)
	}
	// 密文物理落在 <root>/videos/ 下（嵌套子目录是密文根；根下无密文）
	if ents, lerr := os.ReadDir(filepath.Join(root, "videos")); lerr != nil || len(ents) == 0 {
		t.Fatalf("嵌套密文应落 <root>/videos/（len=%d err=%v）", len(ents), lerr)
	}
	rootEntries, _ := os.ReadDir(root)
	for _, e := range rootEntries {
		if e.IsDir() && e.Name() == "videos" {
			continue
		}
		if strings.HasPrefix(e.Name(), ".__") {
			continue
		}
		if e.Name() == "secrets" || e.Name() == "LAYOUT_VERSION" {
			continue
		}
		t.Fatalf("密文逃出子目录（根下出现 %q）", e.Name())
	}
}

// 重启收养（adoptNestedDir 标记）空性/来源校验（Minor1）：
// 空子目录 / 仅本层卷专属形态（纯目录容器、纯秘密文件）→ 收养成功；文件+目录混合（普通
// 用户目录特征，config 误指向既有非 wrapper 目录）→ fail-closed 拒绝，防删除封装卷整流删连带
// 清空误指目录（数据损失窗口）。

// TestResolveNestedTargetFS_Adopt_EmptySubdir 空子目录（本卷无数据）→ 收养成功。
func TestResolveNestedTargetFS_Adopt_EmptySubdir(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例（resolveTargetFS 读取），不并发。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	if err := os.MkdirAll(filepath.Join(root, "w1"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/w1"}}
	if _, err := resolveTargetFS(ctx, v); err != nil {
		t.Fatalf("空子目录重启收养应成功: %v", err)
	}
}

// TestResolveNestedTargetFS_Adopt_ContainerDirsOnly 纯目录容器（secretdata 随机容器 / files
// user 桶形态）→ 收养成功。
func TestResolveNestedTargetFS_Adopt_ContainerDirsOnly(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并发。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	for _, c := range []string{"c1", "c2"} {
		if err := os.MkdirAll(filepath.Join(root, "w2", c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/w2"}}
	if _, err := resolveTargetFS(ctx, v); err != nil {
		t.Fatalf("纯目录容器子目录重启收养应成功: %v", err)
	}
}

// TestResolveNestedTargetFS_Adopt_SecretFilesOnly 纯秘密文件（secrets 钥匙卷形态）→ 收养成功。
func TestResolveNestedTargetFS_Adopt_SecretFilesOnly(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并发。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	base := filepath.Join(root, "w4")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "datakey"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/w4"}}
	if _, err := resolveTargetFS(ctx, v); err != nil {
		t.Fatalf("纯秘密文件子目录重启收养应成功: %v", err)
	}
}

// TestResolveNestedTargetFS_Adopt_MixedReject 文件+目录混合（普通用户目录，config 误指向既有
// 非 wrapper 目录）→ fail-closed 拒绝装配，防止删除封装卷整流删连带清空误指目录。
func TestResolveNestedTargetFS_Adopt_MixedReject(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并发。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	base := filepath.Join(root, "w3")
	if err := os.MkdirAll(filepath.Join(base, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/w3"}}
	if _, err := resolveTargetFS(ctx, v); err == nil {
		t.Fatal("文件+目录混合既有子目录重启收养应 fail-closed 拒绝")
	}
}

// TestIsSelfBuiltNestedEntry 收养判定跳过的本卷自建结构条目（LAYOUT_VERSION 标记文件 + meta
// 桶）命名识别；真实内容（user / secrets 钥匙）不在此列，留给仅同态收养判定。
func TestIsSelfBuiltNestedEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"LAYOUT_VERSION", true},
		{"meta", true},
		{"user", false},      // files 层用户桶（容器形态），不跳过。
		{"datakey", false},   // secrets 卷钥匙文件（纯文件形态），不跳过。
		{"README.md", false}, // 普通用户文件，绝不跳过（混合保护核心）。
	}
	for _, c := range cases {
		if got := isSelfBuiltNestedEntry(c.name); got != c.want {
			t.Errorf("isSelfBuiltNestedEntry(%q)=%v want %v", c.name, got, c.want)
		}
	}
}

// TestResolveNestedTargetFS_Adopt_SecretdataOwnLayout secretdata 卷**自身**落盘结构（嵌套根写
// LAYOUT_VERSION 标记文件 + meta 桶目录 + user 桶）→ 顶层 1 文件 + 多目录 = 混合形态；排除
// 本卷自建结构（LAYOUT_VERSION/meta）后剩余纯目录（user 桶）→ 应收养成功。修复「重启收养误判
// secretdata 卷自身结构 → config 声明嵌套封装卷重启 boot-fail」（复测 CRITICAL）。
func TestResolveNestedTargetFS_Adopt_SecretdataOwnLayout(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并行。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	base := filepath.Join(root, "vault")
	for _, d := range []string{"meta", "user"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// storage.OpenRoot 布局版本标记文件（嵌套 secretdata root 首次启动写）。
	if err := os.WriteFile(filepath.Join(base, "LAYOUT_VERSION"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/vault"}}
	if _, err := resolveTargetFS(ctx, v); err != nil {
		t.Fatalf("secretdata 本卷结构（LAYOUT_VERSION+meta+user）重启收养应成功（修复 boot-fail）: %v", err)
	}
}

// TestResolveNestedTargetFS_Adopt_MixedWithLayoutReject LAYOUT_VERSION + 普通用户文件 + 目录：
// 跳过 LAYOUT_VERSION 后仍「文件+目录混合」→ fail-closed 拒绝（防误删保护对含自建结构条目的
// 普通目录不豁免——用户确实把数据放进子目录时不能因多了个 LAYOUT_VERSION 就被误收养清空）。
func TestResolveNestedTargetFS_Adopt_MixedWithLayoutReject(t *testing.T) {
	// sproxy:serial: 全局 secretDataSet 单例，不并行。
	storeTempSet(t)
	ctx := adoptNestedDir(context.Background())
	root := secretDataSet.Load().Default().RootDir
	base := filepath.Join(root, "w5")
	if err := os.MkdirAll(filepath.Join(base, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "LAYOUT_VERSION"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := volume.Volume{Name: "sd", Extra: map[string]any{"target": "default/w5"}}
	if _, err := resolveTargetFS(ctx, v); err == nil {
		t.Fatal("LAYOUT_VERSION + 普通文件 + 目录混合既有子目录重启收养应 fail-closed 拒绝（防误删）")
	}
}
