// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secrets"
)

// newTestSet 建一个最小 registry.Set（本地默认卷 + 命名空间 metadata），供装配测试用。
// 返回的 Set 需经 t.Cleanup(set.Close) 释放根句柄（Windows 下不放会卡 TempDir 清理）。
func newTestSet(t *testing.T) *registry.Set {
	t.Helper()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	vols := []volume.Volume{{Name: "default", Type: volume.TypeLocal, RootDir: root}}
	roots := map[string]*storage.Root{"default": rt}
	pools := map[string]*quota.Pool{"default": quota.NewPool(0)}
	set := registry.NewSet(vols, roots, map[string]registry.ExternalBackend{}, pools, "default")
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// assembleTestSecretsVolume 装配一个「config 声明形态」的 secrets 卷到 set，返回其 Manager。
//
// 走生产同路径的两步（不触发 `registry.NewBackend` 工厂，故**不依赖**生产注册表/secretDataSet
// 全局单例 ⇒ 用例可并发）：`secretsDirFSFor` 解析 `<root>/secrets` 视图 + `secrets.NewBackend`
// 构造后端，再 `AddExternalVolume` 登记（使其经 `secrets://<name>/...` 可被 ResolveURL 寻址）。
//
// 默认 secrets 卷自 2026-10-15 起是 **per-owner** 的、不是已装配卷（由 `/api/secrets` 按请求
// owner 解析），故凡需要 `secret_url` 密钥来源的用例都必须像生产那样**显式声明**一个 secrets 卷。
func assembleTestSecretsVolume(t *testing.T, ctx context.Context, set *registry.Set, name, root string) *secrets.Manager {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir secrets root %s: %v", root, err)
	}
	v := volume.Volume{Name: name, Type: "secrets", RootDir: root,
		Extra: map[string]any{"target": "local", "root": root}}
	be, err := secrets.NewBackend(ctx, v, secretsDirFSFor(set, root, nil))
	if err != nil {
		t.Fatalf("secrets.NewBackend(%s): %v", name, err)
	}
	if err := set.AddExternalVolume(v, be); err != nil {
		t.Fatalf("AddExternalVolume(%s): %v", name, err)
	}
	mgr := secrets.ManagerOfExternal(be)
	if mgr == nil {
		t.Fatalf("ManagerOfExternal(%s) 反取失败", name)
	}
	return mgr
}
