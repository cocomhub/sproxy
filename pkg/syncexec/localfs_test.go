// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// markerFS 是 Root 感知提供者的替身（仅用于身份断言；FS 方法由嵌入接口占位）。
type markerFS struct{ syncpkg.FS }

// TestExecutor_LocalFS_PrefersRootAwareProvider 评审 P0：本地侧必须优先使用**Root 感知**FS
// 提供者（装配层注入 `NewRootFS(tenantRoot)` ⇒ at-rest 透明加解密 + 封印）；未注入时回落
// 路径式 LocalFS（测试/未接线装配零回归）。
// 变异：把 localFS 改回恒 NewLocalFS → 本用例红（加密卷上将落明文/传密文）。
func TestExecutor_LocalFS_PrefersRootAwareProvider(t *testing.T) {
	t.Parallel()
	marker := &markerFS{}
	e := &Executor{
		TenantRootFS: func(owner string) (syncpkg.FS, bool) {
			if owner == "alice" {
				return marker, true
			}
			return nil, false
		},
	}
	if got := e.localFS("/tmp/whatever", "alice"); got != syncpkg.FS(marker) {
		t.Fatalf("已注入提供者时必须使用 Root 感知 FS（不接受裸 LocalFS），got %T", got)
	}
	// provider 对未知 owner 返回 false → 回落 LocalFS（非 nil）。
	if got := e.localFS(t.TempDir(), "bob"); got == nil {
		t.Fatal("provider 未命中时应回落路径式 LocalFS")
	}
	// 未注入（nil）→ 回落 LocalFS（零回归）。
	e2 := &Executor{}
	if got := e2.localFS(t.TempDir(), "alice"); got == nil {
		t.Fatal("未注入时应回落路径式 LocalFS")
	}
}
