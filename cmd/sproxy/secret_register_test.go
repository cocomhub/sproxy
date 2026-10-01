// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

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
	if werr := be.FS().WriteFile(context.Background(), "k", stringsReader("val"), 3, 0); werr != nil {
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
	if err := setupSecretBackends(ctx, set, localRoot, nil); err != nil {
		t.Fatalf("setupSecretBackends: %v", err)
	}
	// 给 secretdata 卷造密钥：在默认 secrets 卷建一个。
	mgr := secrets.ManagerOfExternal(set.External("default-secrets"))
	if mgr == nil {
		t.Fatal("默认 secrets 卷 ManagerOfExternal 反取失败")
	}
	if _, err := mgr.Create(ctx, "datakey"); err != nil {
		t.Fatalf("Create key: %v", err)
	}
	// 注册 secretdata 后端（secret_url 指向默认 secrets 卷，卷名 default）。
	registerSecretdataOnce = sync.Once{} // 重置 once（测试可重复注册不同 type）
	typ := "secretdata-test"
	registerSecretdataBackendWithFS(typ, defaultSecretdataSecret)
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
	if werr := be.FS().WriteFile(ctx, "a.mp4", stringsReader("hello secret"), 12, 0); werr != nil {
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
