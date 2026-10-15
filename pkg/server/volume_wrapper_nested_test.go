// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_wrapper_nested_test.go 提供「config 声明嵌套封装卷（secretdata + target=<卷>/<子目录>）」
// 的测试装配夹具，与生产 `setupSecretBackends` + `WireConfigNestedWrappers` 顺序/形态一致。
// 被 volume_wrapper_delete_quota_test.go（删除链路配额）与 volume_wrapper_root_list_test.go
// （根目录列举）共用。

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// nestedSecretType 是测试用「推迟装配」的外部卷类型名（避开生产 "secretdata"，防污染共享
// 标记表——本地注册 + t.Cleanup 移除）。
const nestedSecretType = "nested-secretdata-test"

// newNestedSecretWrapperHandlers 装配「config 声明嵌套封装卷」的测试 Handlers：
// main 本地卷（容量 mainCap，0 = 不限）+ 卷 store + config 声明的嵌套封装卷 vault
// （type=nestedSecretType，target=main/videos）→ AttachExternal(真实 secretdata 后端，
// 底层 = SubFS(mainRoot, videos)) + WireConfigNestedWrappers（link + DelegatePool）。
// 与生产 `setupSecretBackends` + `WireConfigNestedWrappers` 的顺序/形态一致。
func newNestedSecretWrapperHandlers(t *testing.T, mainCap int64) *Handlers {
	t.Helper()
	registry.MarkDeferredType(nestedSecretType)
	t.Cleanup(func() { registry.UnmarkDeferredTypeForTest(nestedSecretType) })

	mainDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = mainDir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: mainDir, VolCapacity: ByteSize(mainCap)},
		{Name: "vault", Type: nestedSecretType, Extra: map[string]any{"target": "main/videos"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.SetUserVolumeStore(NewUserVolumeStore(mainDir))

	// 补装外部后端（生产：setupSecretBackends → AttachExternal）。
	base := syncpkg.NewLocalFS(mainDir, nil)
	if err := base.MakeDir(context.Background(), "videos"); err != nil {
		t.Fatalf("MakeDir videos: %v", err)
	}
	sub, err := syncpkg.NewSubFS(base, "videos")
	if err != nil {
		t.Fatalf("NewSubFS: %v", err)
	}
	be, err := secretdata.NewBackend(context.Background(),
		volume.Volume{Name: "vault", Type: nestedSecretType,
			Extra: map[string]any{"target": "main/videos"}}, sub,
		secretdata.Options{Secret: []byte("nested-delete-quota-key-01"), MetaPadBytes: 4096, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("secretdata.NewBackend: %v", err)
	}
	if aerr := h.volSet.AttachExternal("vault", be); aerr != nil {
		t.Fatalf("AttachExternal: %v", aerr)
	}
	if werr := h.WireConfigNestedWrappers(); werr != nil {
		t.Fatalf("WireConfigNestedWrappers: %v", werr)
	}
	return h
}
