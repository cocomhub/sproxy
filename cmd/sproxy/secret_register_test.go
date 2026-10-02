// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// TestVcExtraInt64 验证 vcExtraInt64 解析 extra.meta_pad_bytes 的多种形态：
// 解码器产出 float64（JSON 数字）、Go 内联 map 产出 int64/int，缺省/非数值返回 0。
func TestVcExtraInt64(t *testing.T) {
	t.Parallel()
	const key = "meta_pad_bytes"
	cases := []struct {
		name  string
		extra map[string]any
		want  int64
	}{
		{name: "缺省", extra: map[string]any{}, want: 0},
		{name: "float64-JSON数字", extra: map[string]any{key: float64(1 << 20)}, want: 1 << 20},
		{name: "float64-小数截断", extra: map[string]any{key: float64(1024.9)}, want: 1024},
		{name: "int64", extra: map[string]any{key: int64(4096)}, want: 4096},
		{name: "int", extra: map[string]any{key: 8192}, want: 8192},
		{name: "非数值", extra: map[string]any{key: "1MB"}, want: 0},
		{name: "零值", extra: map[string]any{key: int64(0)}, want: 0},
		{name: "负数忽略", extra: map[string]any{key: int64(-4096)}, want: 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := vcExtraInt64(volume.Volume{Extra: c.extra}, key); got != c.want {
				t.Errorf("vcExtraInt64(%q)=%d, want %d", key, got, c.want)
			}
		})
	}
}

// TestVcExtraBoolAndStrings 验证任务 9d 新增的 extra 解析辅助：vcExtraBool（erasure）与
// vcExtraStrings（targets 副本卷名列表，兼容 []string 与 JSON 解码的 []any）。
func TestVcExtraBoolAndStrings(t *testing.T) {
	t.Parallel()
	// vcExtraBool：true/false/缺省。
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": true}}, "erasure"); !got {
		t.Error("vcExtraBool(true) 应为 true")
	}
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{"erasure": false}}, "erasure"); got {
		t.Error("vcExtraBool(false) 应为 false")
	}
	if got := vcExtraBool(volume.Volume{Extra: map[string]any{}}, "erasure"); got {
		t.Error("vcExtraBool(缺省) 应为 false")
	}
	// vcExtraStrings：[]string 直通、[]any 兼容、空项过滤、缺省 nil。
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []string{"a", "b"}}}, "targets"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("vcExtraStrings([]string)=%v", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []any{"replica1", float64(2)}}}, "targets"); len(got) != 1 || got[0] != "replica1" {
		t.Errorf("vcExtraStrings([]any)=%v（应过滤非字符串）", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{"targets": []string{"", "x"}}}, "targets"); len(got) != 1 || got[0] != "x" {
		t.Errorf("vcExtraStrings 空串应过滤：%v", got)
	}
	if got := vcExtraStrings(volume.Volume{Extra: map[string]any{}}, "targets"); got != nil {
		t.Errorf("vcExtraStrings(缺省) 应为 nil，got %v", got)
	}
}

// TestEnsureDefaultSecretsVolume 启动默认建本地卷作默认 secrets 卷（§9.1）。
func TestEnsureDefaultSecretsVolume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	root := t.TempDir()
	mgr, err := ensureDefaultSecretsVolume(ctx, set, root, nil)
	if err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	if mgr == nil {
		t.Fatal("返回 nil Manager")
	}
	if set.External("default-secrets") == nil {
		t.Fatal("默认 secrets 卷未挂到 Set.External")
	}
	// 通过 Manager 创建并读回一个 secret，验证链路可用。
	if _, cerr := mgr.Create(ctx, "testkey"); cerr != nil {
		t.Fatalf("Create: %v", cerr)
	}
	if got, rerr := mgr.Read(ctx, "testkey"); rerr != nil || len(got) == 0 {
		t.Errorf("Read: got=%d len err=%v", len(got), rerr)
	}
	// 默认 secrets 卷应恰落 <root>/secrets（设计 §6.1/§9），不得双重 append 成
	// <root>/secrets/secrets。
	if _, serr := os.Stat(filepath.Join(root, "secrets", "testkey")); serr != nil {
		t.Errorf("默认 secrets 卷未落 <root>/secrets/testkey: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, "secrets", "secrets", "testkey")); serr == nil {
		t.Error("默认 secrets 卷错误落到 <root>/secrets/secrets（双重 append）")
	}
	// 幂等：重复调用不报错、返回同一 Manager（读取同一密钥）。
	mgr2, err := ensureDefaultSecretsVolume(ctx, set, root, nil)
	if err != nil {
		t.Fatalf("二次装配: %v", err)
	}
	if got, err := mgr2.Read(ctx, "testkey"); err != nil || len(got) == 0 {
		t.Errorf("二次装配读回: err=%v", err)
	}
}

// TestRegisterSecretsBackendWithFS 注册 secrets backend 类型后 registry 可分派构造（含底层 FS 注入）。
func TestRegisterSecretsBackendWithFS(t *testing.T) {
	t.Parallel()
	typ := "secrets-test"
	root := t.TempDir()
	if err := os.MkdirAll(root+"/secrets", 0o700); err != nil {
		t.Fatal(err)
	}
	registerSecretsBackendWithFS(typ, func(ctx context.Context, v volume.Volume) (syncpkg.FS, error) {
		return secretsLocalFS(root), nil
	})
	v := volume.Volume{Name: "sv", Type: typ, RootDir: root, Extra: map[string]any{"target": "local", "root": root}}
	be, err := registry.NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if be == nil || be.FS() == nil {
		t.Fatal("backend 或 FS 为 nil")
	}
	// 用 FS 真跑一次：写 + 读。
	if werr := be.FS().WriteFile(context.Background(), "k", strings.NewReader("val"), 3, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(context.Background(), "k")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf := make([]byte, 3)
	_, _ = rc.Read(buf)
	rc.Close()
	if string(buf) != "val" {
		t.Errorf("内容=%q", buf)
	}
}

// TestSetupSecretBackends_Secretdata 装配 secrets + secretdata，验证：
//   - 默认 secrets 卷已建；
//   - secretdata backend 经 secret_url 读到密钥、底层 target=local 落本地；
//   - 通过 secretdata FS 写读 files 可透明加解密。
func TestSetupSecretBackends_Secretdata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	localRoot := t.TempDir()
	if _, err := ensureDefaultSecretsVolume(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("ensureDefaultSecretsVolume: %v", err)
	}
	// 生产装配 setupSecretBackends 会调用 registerSecretsBackend()（声明协议 "secrets"，
	// 供 secret_url 的 ResolveURL 寻址）。此处显式调用使本测试**自包含**——不依赖
	// 其它测试触发 registerSecretsOnce 的套件顺序（隔离运行/-shuffle 也能通过）。
	registerSecretsBackend()
	// 给 secretdata 卷造密钥：在默认 secrets 卷建一个。
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	// 手动注册 secretdata 后端（独立类型名，闭包捕获本测试的 set——不依赖全局
	// setupSecretdataOnce 的共享 set，避免跨测试残留）。
	typ := "secretdata-test"
	registerSecretdataBackendWithFS(typ, func(ctx context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(ctx, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
	dataRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: typ, RootDir: dataRoot, Extra: map[string]any{
		"target":     "local",
		"root":       dataRoot,
		"secret_url": "secrets://default/datakey",
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	// 直接 WriteFile/OpenRead 透明加解密。
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
}

// TestSetupSecretBackends_Secretdata_MultiTarget（任务 9d 修复轮 Imp-1）：extra.targets
// 多 local root → 生产多 target 装配生效——装配层解析副本 local root 构造副本底层 FS →
// 写后主/副本两 root 都有同一容器（副本复制运行，非仅记账）。跨外部卷接线留后续片。
func TestSetupSecretBackends_Secretdata_MultiTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	set := newTestSet(t)
	if _, err := ensureDefaultSecretsVolume(ctx, set, t.TempDir(), nil); err != nil {
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
	typ := "secretdata-multitarget"
	registerSecretdataBackendWithFS(typ, func(ctx context.Context, v volume.Volume) ([]byte, error) {
		return defaultSecretdataSecret(ctx, v, set)
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
	primaryRoot := t.TempDir()
	replicaRoot := t.TempDir()
	v := volume.Volume{Name: "sd", Type: typ, RootDir: primaryRoot, Extra: map[string]any{
		"target":     "local",
		"root":       primaryRoot,
		"secret_url": "secrets://default/datakey",
		"targets":    []string{replicaRoot},
	}}
	be, err := registry.NewBackend(ctx, v)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if werr := be.FS().WriteFile(ctx, "a.mp4", strings.NewReader("hello secret"), 12, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := be.FS().OpenRead(ctx, "a.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if string(buf) != "hello secret" {
		t.Errorf("还原=%q", buf)
	}
	// 副本复制生效：主/副本两 root 都应含同一容器目录（非仅记账）。
	primaryDirs := rootContainerDirs(t, primaryRoot)
	replicaDirs := rootContainerDirs(t, replicaRoot)
	if len(primaryDirs) != 1 {
		t.Fatalf("主 target 应含 1 个容器目录，got %+v", primaryDirs)
	}
	if len(replicaDirs) != 1 {
		t.Fatalf("副本 target 应含 1 个容器目录（副本复制未生效），got %+v", replicaDirs)
	}
	if primaryDirs[0] != replicaDirs[0] {
		t.Errorf("主/副本容器目录名不一致：%q vs %q", primaryDirs[0], replicaDirs[0])
	}
}

// rootContainerDirs 列出本地 root 下的容器目录名（复制验证用）。
func rootContainerDirs(t *testing.T, root string) []string {
	t.Helper()
	fs := syncpkg.NewLocalFS(root, nil)
	entries, err := fs.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir(%s): %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e.Name)
		}
	}
	return out
}
