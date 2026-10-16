// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_usage_test.go —— 卷用量**单一查询面**（`Handlers.volumeUsage`）回归。
//
// 背景（用户裁定 2026-10-16「QuotaVolume 接线到真实消费者」）：此前 `volumeStatusMeta`
// 与 `volumeUsageOf` 各持一份重复逻辑；现收敛为 `volumeUsage`，并以**配额层为第一优先级**。
//
// 判别力设计：装配时给配额层一个**独立于卷容量池**的计数器（`capacity.NewCounter` 持久化文件态）
// ⇒ 若 `volumeUsage` 真的走配额层，返回值 = 计数器值（≠ 池值）；若退回旧逻辑 ⇒ 返回池值 ⇒ 红。

import (
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
)

// bareBE 是仅满足 registry.ExternalBackend 的最小替身。
type bareBE struct{ fs syncpkg.FS }

func (b bareBE) FS() syncpkg.FS { return b.fs }
func (b bareBE) Close() error   { return nil }

// TestVolumeUsage_PrefersQuotaLayer 断言卷用量经**配额层**读取（而非各自拼池 + UsageProvider）。
func TestVolumeUsage_PrefersQuotaLayer(t *testing.T) {
	t.Parallel()
	h := newNestedSecretWrapperHandlers(t, 0) // 该夹具已装配 h.volSet ✓
	const volName = "vq"

	// 容量池**故意不装配** ⇒ 判别力最强：走配额层 ⇒ 4096；退回旧逻辑 ⇒ 0（无池）。
	pool := quota.NewPool(1 << 20)

	// 配额层用**独立计数器**（持久化文件态）：已用 4096。
	counter := capacity.NewCounter(1<<20, filepath.Join(t.TempDir(), "vq.capacity"))
	if err := counter.TryAdd(4096); err != nil {
		t.Fatalf("TryAdd: %v", err)
	}
	wrapped := capacity.WrapStorageVolume(volName, bareBE{fs: syncpkg.NewLocalFS(t.TempDir(), nil)}, counter, nil, pool)
	if be := h.volSet.External(volName); be == nil {
		if err := h.volSet.AttachExternal(volName, wrapped); err != nil {
			t.Fatalf("AttachExternal: %v", err)
		}
	}

	if got := h.volumeUsage(volume.Volume{Name: volName, Type: "s3"}); got != 4096 {
		t.Fatalf("volumeUsage=%d，want 4096（应取**配额层**计数器值；0 表示退回旧池逻辑）", got)
	}
	if q := h.volumeQuotaLayer(volName); q == nil {
		t.Fatal("配额层应可达（经唯一入口装配 ⇒ capacity.Backend.Quota() 非 nil）")
	}
}
