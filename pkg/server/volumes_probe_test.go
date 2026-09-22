// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// registerFakeSFTPBackend 注册一个返回 fake backend 的构造器（探针测试用）。
// 类型名带唯一后缀避免与生产 "sftp" 重复；t.Cleanup 反注册防污染共享注册表。
// registerFakeSFTPBackend 注册探针可编程的 fake 构造器（探针测试用）。
// pingErr 由测试持有指针；probe=false 时构造无探针后端（unknown 测试）。
func registerFakeSFTPBackend(t *testing.T, typ string, probe bool, pingErr *error) {
	t.Helper()
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		if !probe {
			return &noProbeBackend{}, nil
		}
		return &fakeExternalBackend{pingErr: *pingErr}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })
}

// noProbeBackend 不实现 registry.HealthProbe（探针能力缺失 → unknown 状态）。
type noProbeBackend struct{}

func (b *noProbeBackend) FS() syncpkg.FS { return nil }
func (b *noProbeBackend) Close() error   { return nil }

// fakeExternalBackend 是探针测试用外部后端：Ping 行为可编程（healthy/degraded）。
type fakeExternalBackend struct {
	fs      syncpkg.FS
	pingErr error
}

func (b *fakeExternalBackend) FS() syncpkg.FS { return b.fs }
func (b *fakeExternalBackend) Close() error   { return nil }

var _ registry.HealthProbe = (*fakeExternalBackend)(nil)

func (b *fakeExternalBackend) Ping(ctx context.Context) error {
	return b.pingErr
}

// TestVolumeStatus_ExternalProbeDegraded 验证外部卷探针：
// 后端 Ping 失败 → VolumeStatus.State=degraded（可观测）。
func TestVolumeStatus_ExternalProbeDegraded(t *testing.T) {
	t.Parallel()
	pingErr := fmt.Errorf("连接被拒")
	registerFakeSFTPBackend(t, "sfx-deg", true, &pingErr)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: t.TempDir(), Type: "local"},
		{Name: "sftp1", Root: t.TempDir(), Type: "sfx-deg", Extra: map[string]any{
			"url": "sftp://u@127.0.0.1:22/", "password": "x",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	ts := httptest.NewServer(volumesAPIWrap(h, "alice"))
	defer ts.Close()
	code, out := listVolumes(t, ts.URL)
	if code != 200 {
		t.Fatalf("GET /api/volumes = %d, want 200", code)
	}
	if len(out.Volumes) != 2 {
		t.Fatalf("应 2 个卷, got %d", len(out.Volumes))
	}
	for _, v := range out.Volumes {
		switch v.Name {
		case "sftp1":
			if v.State != volumeStateDegraded {
				t.Fatalf("sftp1 探针失败应 degraded, got %q", v.State)
			}
		case "main":
			if v.State != volumeStateHealthy {
				t.Fatalf("本地卷 main 应 healthy, got %q", v.State)
			}
		}
	}
}

// TestVolumeStatus_ExternalProbeHealthy 验证探针通过 → healthy。
func TestVolumeStatus_ExternalProbeHealthy(t *testing.T) {
	t.Parallel()
	var pingErr error // nil = 探针通过
	registerFakeSFTPBackend(t, "sfx-health", true, &pingErr)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: t.TempDir(), Type: "local"},
		{Name: "sftp1", Root: t.TempDir(), Type: "sfx-health", Extra: map[string]any{
			"url": "sftp://u@127.0.0.1:22/", "password": "x",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	ts := httptest.NewServer(volumesAPIWrap(h, "alice"))
	defer ts.Close()
	_, out := listVolumes(t, ts.URL)
	for _, v := range out.Volumes {
		if v.Name == "sftp1" && v.State != volumeStateHealthy {
			t.Fatalf("sftp1 探针通过应 healthy, got %q", v.State)
		}
	}
}

// TestVolumeStatus_ExternalNoProbeUnknown 验证后端未实现 HealthProbe → unknown（不误报）。
func TestVolumeStatus_ExternalNoProbeUnknown(t *testing.T) {
	t.Parallel()
	var pingErr error
	registerFakeSFTPBackend(t, "sfx-unk", false, &pingErr)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: t.TempDir(), Type: "local"},
		{Name: "sftp1", Root: t.TempDir(), Type: "sfx-unk", Extra: map[string]any{
			"url": "sftp://u@127.0.0.1:22/", "password": "x",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	ts := httptest.NewServer(volumesAPIWrap(h, "alice"))
	defer ts.Close()
	_, out := listVolumes(t, ts.URL)
	for _, v := range out.Volumes {
		if v.Name == "sftp1" && v.State != volumeStateUnknown {
			t.Fatalf("无探针后端应 unknown, got %q", v.State)
		}
	}
}
