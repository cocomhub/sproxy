// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ftp

// ftp_backend_test.go 钉住 FTP 后端注册与配置校验（V3 plugin）：
// RegisterFTPBackend 注册后 registry.BackendTypes() 含 "ftp"（GET /api/backends 数据源）；
// newFTPBackend 配置校验（缺 url / 缺认证 → fail-closed）。

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestFTPBackend_RegisterAndBackends 验证 RegisterFTPBackend 注册后：
// registry.BackendTypes() 含 "ftp"（GET /api/backends 数据源）。
func TestFTPBackend_RegisterAndBackends(t *testing.T) {
	t.Parallel()
	RegisterFTPBackend()
	found := false
	for _, typ := range registry.BackendTypes() {
		if typ == "ftp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("registry.BackendTypes() 应含 ftp, got %v", registry.BackendTypes())
	}
}

// TestFTPBackend_NewBackendConfigValidation 验证 newFTPBackend 配置校验：
// 缺 url / 缺认证 → 构造失败（fail-closed）。
func TestFTPBackend_NewBackendConfigValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// 缺 url。
	_, err := newFTPBackend(ctx, volume.Volume{Name: "v1", Type: "ftp", Extra: map[string]any{"password": "x"}})
	if err == nil {
		t.Fatalf("缺 url 应构造失败")
	}
	// 缺认证。
	_, err = newFTPBackend(ctx, volume.Volume{Name: "v1", Type: "ftp", Extra: map[string]any{"url": "ftp://127.0.0.1:21"}})
	if err == nil {
		t.Fatalf("缺认证应构造失败（fail-closed）")
	}
	// url scheme 非法。
	_, err = newFTPBackend(ctx, volume.Volume{Name: "v1", Type: "ftp", Extra: map[string]any{"url": "http://h:21", "password": "x"}})
	if err == nil {
		t.Fatalf("非 ftp scheme 应构造失败")
	}
	// 本地卷类型拒绝。
	_, err = newFTPBackend(ctx, volume.Volume{Name: "v1", Type: "", Extra: nil})
	if err == nil {
		t.Fatalf("本地卷类型应拒绝")
	}
}

// TestFTPBackend_ImplementsHealthProbe 验证 ftpExternalBackend 实现
// registry.HealthProbe（探针接线：server 层断言此接口驱动 degraded 状态）。
func TestFTPBackend_ImplementsHealthProbe(t *testing.T) {
	t.Parallel()
	fs := newTestFTPFS(t, nil)
	be := &ftpExternalBackend{fs: fs}
	var _ registry.HealthProbe = be // 编译期断言
	if err := be.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 应 healthy: %v", err)
	}
}
