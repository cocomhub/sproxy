// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sftp

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestSFTPBackend_RegisterAndBackends 验证 RegisterSFTPBackend 注册后：
// registry.BackendTypes() 含 "sftp"（GET /api/backends 数据源）。
func TestSFTPBackend_RegisterAndBackends(t *testing.T) {
	t.Parallel()
	RegisterSFTPBackend()
	found := false
	for _, typ := range registry.BackendTypes() {
		if typ == "sftp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry.BackendTypes() 应含 sftp, got %v", registry.BackendTypes())
	}
}

// TestSFTPBackend_NewBackendConfigValidation 验证 newSFTPBackend 配置校验：
// 缺 url / 缺认证 → 构造失败（fail-closed）。
func TestSFTPBackend_NewBackendConfigValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 缺 url。
	_, err := newSFTPBackend(ctx, volume.Volume{Name: "v1", Type: "sftp", Extra: map[string]any{"password": "x"}})
	if err == nil {
		t.Fatalf("缺 url 应构造失败")
	}
	// 缺认证。
	_, err = newSFTPBackend(ctx, volume.Volume{Name: "v1", Type: "sftp", Extra: map[string]any{"url": "sftp://u@127.0.0.1:22"}})
	if err == nil {
		t.Fatalf("缺认证应构造失败（fail-closed）")
	}
	// 本地卷类型拒绝。
	_, err = newSFTPBackend(ctx, volume.Volume{Name: "v1", Type: "", Extra: nil})
	if err == nil {
		t.Fatalf("本地卷类型应拒绝")
	}
}

// TestSFTPBackend_ImplementsHealthProbe 验证 sftpExternalBackend 实现
// registry.HealthProbe（探针接线：server 层断言此接口驱动 degraded 状态）。
func TestSFTPBackend_ImplementsHealthProbe(t *testing.T) {
	t.Parallel()
	fs := newTestSFTPClient(t, nil)
	be := &sftpExternalBackend{fs: fs}
	var _ registry.HealthProbe = be // 编译期断言
	if err := be.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应 healthy: %v", err)
	}
}
