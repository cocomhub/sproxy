// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

// local_backend_registration_test.go —— M1b：`local` 注册为**普通后端** + 用户可创建面守卫。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。M1b 的三条不变量：
//  1. `local` **可注册**（`RegisterBackend` 不再拒绝）且 `NewBackend` 可统一分派（`""` 归一为 local）；
//  2. **注册 ≠ 用户可创建**：`IsUserCreatableBackendType` 排除 local/空串 ⇒ 建卷 API 行为不变 ✓
//     （否则调用方可指定任意 `RootDir` ⇒ 路径/权限面 ✗）；
//  3. `BackendSchemas()` 排除 local ⇒ 建卷表单类型下拉零行为变更 ✓。
//
// **单测而非 3 个**：注册表是全局共享状态 ⇒ 用例不能并行（serial 预算棘轮）；合并为一个用例
// 集中登记/清理（`UnregisterBackendForTest`）。
//
// 变异：`IsUserCreatableBackendType` 去掉 local 判断 ⇒ 断言红；`BackendSchemas` 去掉过滤 ⇒ 断言红；
// `NewBackend` 恢复硬拒 local ⇒ 分派断言红。

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

func TestLocalBackendRegistration_AllowedButNotUserCreatable(t *testing.T) {
	const typ = volume.TypeLocal
	if IsBackendRegistered(typ) {
		t.Fatal("前置：测试环境不应预先注册 local（装配层才注册）")
	}
	RegisterBackend(typ, func(_ context.Context, v volume.Volume) (ExternalBackend, error) {
		return fakeStorageVolume{name: v.Name, fs: syncpkg.NewLocalFS(t.TempDir(), nil)}, nil
	})
	t.Cleanup(func() { UnregisterBackendForTest(typ) })

	// ① 可注册 + 可统一分派（显式 local 与空串归一）。
	if !IsBackendRegistered(typ) {
		t.Fatal("注册后 IsBackendRegistered(local) 应为 true")
	}
	for _, tv := range []string{volume.TypeLocal, ""} {
		be, err := NewBackend(context.Background(), volume.Volume{Name: "v", Type: tv})
		if err != nil {
			t.Fatalf("NewBackend(type=%q) 应统一分派到 local 后端: %v", tv, err)
		}
		if be == nil {
			t.Fatalf("NewBackend(type=%q) 返回 nil", tv)
		}
	}

	// ② 注册 ≠ 用户可创建（建卷 API / 预签名面守卫）。
	if IsUserCreatableBackendType(typ) {
		t.Fatal("local 不得为用户可创建类型（否则可指定任意 RootDir ⇒ 路径/权限面）")
	}
	if IsUserCreatableBackendType("") {
		t.Fatal("空串（等义 local）不得为用户可创建类型")
	}
	const extTyp = "test-m1b-user-creatable"
	RegisterBackend(extTyp, func(context.Context, volume.Volume) (ExternalBackend, error) { return nil, nil })
	t.Cleanup(func() { UnregisterBackendForTest(extTyp) })
	if !IsUserCreatableBackendType(extTyp) {
		t.Fatal("已注册的外部类型应为用户可创建")
	}
	if IsUserCreatableBackendType("no-such-backend-type") {
		t.Fatal("未注册类型不得为用户可创建")
	}

	// ③ 建卷表单类型清单不含 local（零行为变更）。
	for _, s := range BackendSchemas() {
		if s.Type == typ {
			t.Fatal("BackendSchemas 不得包含 local（建卷表单类型下拉零行为变更）")
		}
	}
}
