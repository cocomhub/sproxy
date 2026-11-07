// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// placeholder_adaptive_test.go 钉住「未知大小任务的存储占位必须**自适应**，不得用固定 1 GiB
// 吃光额度」（独立审核 P3，2026-10-18）。
//
// 缺陷：`cloudReservePlaceholder = 1 GiB` 是**固定常量** ✗ —— 批量/组下载（MaxBatchURLs 默认 100）
// 创建 N 条未知大小任务时各占 1 GiB ⇒ 50 MiB/GiB 级上限下，**第 1 条就 507** ⇒ 合法的中小文件
// 下载被拒（用户可见故障），而占位本就只是估计（完成/失败时按磁盘真值收敛）。
//
// 修法：`placeholderFor()` = min(1 GiB, 可用空间 / 并发槽)，下限 16 MiB ⇒ 单任务不独占、
// N 条并发不耗尽额度；真正没空间时仍 fail-closed。

import (
	"fmt"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudDownloadManager_UnknownSizeBatchDoesNotExhaustBudget 未知大小批量创建必须全部成功。
func TestCloudDownloadManager_UnknownSizeBatchDoesNotExhaustBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const maxBytes = int64(1 << 30) // 1 GiB：旧固定占位（1 GiB）在第 2 条即耗尽
	sm := capacity.NewStorageManager(dir, maxBytes, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	cfg.MaxConcurrent = 3
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
	t.Cleanup(mgr.Close)

	// 10 条**未知大小**（totalSize=0）任务：旧实现第 1 条占满 1 GiB、第 2 条即 507。
	for i := range 10 {
		if _, err := mgr.CreateTask("url",
			fmt.Sprintf("http://127.0.0.1:1/unknown-%d.bin", i),
			fmt.Sprintf("u%d.bin", i), 0, "", TaskParams{Save: true}); err != nil {
			t.Fatalf("第 %d 条未知大小任务被拒（未修必红：固定 1 GiB 占位吃光 %d 字节额度）：%v",
				i+1, maxBytes, err)
		}
	}
	// 占位必须被**分摊**（≤ 可用空间/并发槽 + 下限），不得是 1 GiB。
	limit := int64(maxBytes)/int64(cfg.MaxConcurrent) + cloudReservePlaceholderMin
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	for id, task := range mgr.tasks {
		if task.ReservedSize > limit {
			t.Fatalf("任务 %s 占位=%d 超过「可用空间/并发槽 + 下限」=%d（占位未自适应）",
				id, task.ReservedSize, limit)
		}
	}
}

// TestCloudDownloadManager_PlaceholderFor 纯函数：上界 / 分摊 / 下限三态。
func TestCloudDownloadManager_PlaceholderFor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<40, nil, testLogger()) // 1 TiB：充足
	cfg := defaultCloudDownloadConfig()
	cfg.MaxConcurrent = 4
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
	t.Cleanup(mgr.Close)
	if got := mgr.placeholderFor(); got != cloudReservePlaceholder {
		t.Fatalf("空间充足时占位=%d, want 上界 %d", got, cloudReservePlaceholder)
	}

	dir2 := t.TempDir()
	sm2 := capacity.NewStorageManager(dir2, 64*1024*1024, nil, testLogger()) // 64 MiB
	mgr2, _ := newCloudTestManager(t, dir2, sm2, cfg)
	t.Cleanup(mgr2.Close)
	if got, want := mgr2.placeholderFor(), int64(64*1024*1024)/4; got != want {
		t.Fatalf("紧张预算下占位=%d, want %d（可用空间/并发槽）", got, want)
	}

	dir3 := t.TempDir()
	sm3 := capacity.NewStorageManager(dir3, 1, nil, testLogger()) // 几乎没空间
	mgr3, _ := newCloudTestManager(t, dir3, sm3, cfg)
	t.Cleanup(mgr3.Close)
	if got := mgr3.placeholderFor(); got != cloudReservePlaceholderMin {
		t.Fatalf("极小预算下占位=%d, want 下限 %d", got, cloudReservePlaceholderMin)
	}
}
