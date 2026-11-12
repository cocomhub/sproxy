// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"testing"
)

// dirmeta_forward_test.go 钉住**透明装饰器必须转发 `DirMetaCleaner`**（门禁 R21 的运行时对侧）：
// 底层支持 ⇒ 透传（含原样容器路径与返回值）；底层不支持 ⇒ 断言**仍为真**（装饰器无条件实现，
// 这是 R21 要求的形式），调用返回 `ErrUnsupported` 交由调用方按"未实现"处置。

// fakeDirMetaCleaner 只实现 FS 基础面 + 目录 meta 清理能力（嵌入接口省掉无关方法）。
type fakeDirMetaCleaner struct {
	FS
	containers []string
	removed    int
	err        error
}

func (f *fakeDirMetaCleaner) CleanupStaleDirMeta(_ context.Context, container string) (int, error) {
	f.containers = append(f.containers, container)
	return f.removed, f.err
}

// TestPrefixFSForwardsDirMetaCleaner 锁定 PrefixFS 中间层透传：底层支持 ⇒ 断言成立、
// 容器路径**原样**下探、返回值原样回传。
func TestPrefixFSForwardsDirMetaCleaner(t *testing.T) {
	t.Parallel()
	inner := &fakeDirMetaCleaner{removed: 2}
	pf := NewPrefixFS(inner, "pre/")

	c, ok := AssertDirMetaCleaner(pf)
	if !ok {
		t.Fatal("装饰器应无条件实现 DirMetaCleaner（透明转发）")
	}
	n, err := c.CleanupStaleDirMeta(t.Context(), "OypE/abc/C1")
	if err != nil || n != 2 {
		t.Fatalf("应透传底层结果，got (%d, %v)", n, err)
	}
	if len(inner.containers) != 1 || inner.containers[0] != "OypE/abc/C1" {
		t.Fatalf("容器路径应原样下探，got %v", inner.containers)
	}
}

// TestPrefixFSDirMetaCleanerUnsupported 锁定底层无该能力时的口径：断言仍成立（装饰器无条件
// 实现），但调用返回 ErrUnsupported —— 调用方据此按"未实现"处置，而**不是**让断言静默失败。
func TestPrefixFSDirMetaCleanerUnsupported(t *testing.T) {
	t.Parallel()
	c, ok := AssertDirMetaCleaner(NewPrefixFS(&fakePlainFS{}, "pre/"))
	if !ok {
		t.Fatal("装饰器应无条件实现 DirMetaCleaner（透明转发）")
	}
	if n, err := c.CleanupStaleDirMeta(t.Context(), "c1"); n != 0 || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("底层未实现应返回 ErrUnsupported，got (%d, %v)", n, err)
	}
}

// TestStagingQuotaGateFSForwardsDirMetaCleaner 锁定第二个位于 pkg/sync 链上的装饰器同样转发
// （门禁 R21 要求 11 个装饰器全量转发；此处在 pkg/sync 内可覆盖 2 个）。
func TestStagingQuotaGateFSForwardsDirMetaCleaner(t *testing.T) {
	t.Parallel()
	inner := &fakeDirMetaCleaner{removed: 1}
	gate := WrapStagingQuota(inner, nil)

	c, ok := AssertDirMetaCleaner(gate)
	if !ok {
		t.Fatal("staging 门卫应无条件实现 DirMetaCleaner（透明转发）")
	}
	if n, err := c.CleanupStaleDirMeta(t.Context(), "c1"); err != nil || n != 1 {
		t.Fatalf("应透传底层结果，got (%d, %v)", n, err)
	}
	if len(inner.containers) != 1 {
		t.Fatalf("应转发到内层一次，got %v", inner.containers)
	}
}

// TestAssertDirMetaCleaner 能力探测：实现者 true，其它 false（含 nil）。
func TestAssertDirMetaCleaner(t *testing.T) {
	t.Parallel()
	if _, ok := AssertDirMetaCleaner(&fakeDirMetaCleaner{}); !ok {
		t.Fatal("实现者应探测为 true")
	}
	if _, ok := AssertDirMetaCleaner(&fakePlainFS{}); ok {
		t.Fatal("未实现者应为 false")
	}
	if _, ok := AssertDirMetaCleaner(nil); ok {
		t.Fatal("nil 应为 false")
	}
}
