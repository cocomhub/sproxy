// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestExternalVolume_CapacityEnforcedAtFSLayer 钉住「配置外部卷卷级容量在 FS 层强制」：
// 所有经 be.FS() 的写路径共享同一卷级计数器——首次写入占额度，超限的后续写入被拒绝，
// 删除释放；池 Usage 与 UsageProvider 同源（路由/指标一致）。
func TestExternalVolume_CapacityEnforcedAtFSLayer(t *testing.T) {
	t.Parallel()
	typ := fmt.Sprintf("capacity-ext-%d", time.Now().UnixNano())
	inner := &extFS{files: map[string]string{}}
	registry.RegisterBackend(typ, func(context.Context, volume.Volume) (registry.ExternalBackend, error) {
		return &extBackend{fs: inner}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "ext", Type: typ, VolCapacity: ByteSize(100)},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)

	be := h.volSet.External("ext")
	if be == nil {
		t.Fatal("外部卷 ext 未装配")
	}
	fs := be.FS()
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(make([]byte, 60)), 60, 0); err != nil {
		t.Fatalf("写入 60 应成功: %v", err)
	}
	if err := fs.WriteFile(ctx, "b.bin", bytes.NewReader(make([]byte, 60)), 60, 0); err == nil {
		t.Fatal("第二次写 60 超限（120>100）应被拒绝")
	}
	if got := h.volSet.Pool("ext").Usage(); got != 60 {
		t.Fatalf("卷池 Usage = %d, want 60（FS 层记账与 volSet.Pool 同源）", got)
	}
	up, ok := be.(registry.UsageProvider)
	if !ok || up.Usage() != 60 || up.Capacity() != 100 {
		t.Fatalf("UsageProvider = (%v,%v,%v), want (true,60,100)", ok, up, up)
	}
	// 删除释放（extFS.Delete 不真删，但记账按 stat 释放）。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := h.volSet.Pool("ext").Usage(); got != 0 {
		t.Fatalf("删除后卷池 Usage = %d, want 0", got)
	}
}
