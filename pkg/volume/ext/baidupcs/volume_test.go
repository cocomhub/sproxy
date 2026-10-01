// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewVolumeBackend_Basic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	vb, err := NewVolumeBackend(ctx, VolumeBackendConfig{
		Name:    "baidu1",
		Storage: newFakeStorage(),
	})
	if err != nil {
		t.Fatalf("NewVolumeBackend: %v", err)
	}
	if vb.Name != "baidu1" {
		t.Fatalf("Name = %q, want baidu1", vb.Name)
	}
	if vb.FS == nil {
		t.Fatal("FS 不应为 nil")
	}
	if vb.RootDir == "" {
		t.Fatal("RootDir 不应为空")
	}
	// RootDir 应存在（本地中间态布局）
	if _, err := os.Stat(vb.RootDir); err != nil {
		t.Fatalf("RootDir 应存在: %v", err)
	}
}

func TestNewVolumeBackend_LocalRoot(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "baidu-local")
	vb, err := NewVolumeBackend(context.Background(), VolumeBackendConfig{
		Name:      "baidu2",
		Storage:   newFakeStorage(),
		LocalRoot: local,
	})
	if err != nil {
		t.Fatalf("NewVolumeBackend: %v", err)
	}
	if vb.RootDir != local {
		t.Fatalf("RootDir = %q, want %q", vb.RootDir, local)
	}
}

func TestNewVolumeBackend_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := NewVolumeBackend(context.Background(), VolumeBackendConfig{Storage: newFakeStorage()}); err == nil {
		t.Fatal("空 Name 应报错")
	}
	if _, err := NewVolumeBackend(context.Background(), VolumeBackendConfig{Name: "x"}); err == nil {
		t.Fatal("空 Storage 应报错")
	}
}
