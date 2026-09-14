// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// defaultCloudDownloadConfig 是云任务测试的默认配置（与 pkg/cloud 测试内的同名辅助**逐字一致**：
// 含 AllowPrivate=true，否则指向 127.0.0.1 的 httptest 源会被 SSRF 策略拒绝）。
func defaultCloudDownloadConfig() *cloud.CloudDownloadConfig {
	return &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024, // 20 MiB
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
}

// setTestOwnerQuota 设置 owner 的租户配额上限（供云任务配额用例）。
func setTestOwnerQuota(h *Handlers, owner string, bytes int64) {
	cfg := h.cfgPtr.Load()
	if cfg.OwnerQuotas == nil {
		cfg.OwnerQuotas = make(map[string]int64)
	}
	cfg.OwnerQuotas[owner] = bytes
}

// waitTaskDone 轮询等待任务进入终态（completed/failed/cancelled），超时即 Fatal。
// pkg/cloud 侧另有同构副本（测试辅助不能跨包共享：领域包不得反向依赖装配层）。
func waitTaskDone(t *testing.T, mgr *cloud.CloudDownloadManager, id string) {
	t.Helper()
	var last string
	testutil.WaitFor(t, 10*time.Second, func() bool {
		cur, ok := mgr.SnapshotTask(id, "")
		if !ok {
			t.Fatal("task not found")
		}
		last = cur.Status
		switch cur.Status {
		case "completed", "failed", "cancelled":
			return true
		default:
			return false
		}
	}, func() string { return fmt.Sprintf("task %s 未到终态，最后观测 %s", id, last) })
}

// newCloudTestManager 创建 cloud.CloudDownloadManager，装配基于 storageRoot 的租户解析闭包。
// 复用 newAssemblyTestHandlers 提供的 tenantFor/checksumStoreFor/listTenantIDs，
// 使测试无需手工构造 TenantResolver。返回 (manager, 配套 Handlers)；Handlers 仅供测试
// 读取 per-tenant checksum store / 验证租户根等。
func newCloudTestManager(t *testing.T, storageRoot string, sm *capacity.StorageManager, cfg *cloud.CloudDownloadConfig) (*cloud.CloudDownloadManager, *Handlers) {
	t.Helper()
	h := newAssemblyTestHandlers(t, storageRoot)
	mgr := cloud.NewCloudDownloadManager(storageRoot, cloudStorageManager{m: sm}, h.tenantFor, h.checksumStoreFor, h.listTenantIDs, testLogger(), cfg, func(owner string) *quota.Scope {
		return h.quotaBucketFor(owner, "cloud")
	})
	h.cloudMgr = mgr
	t.Cleanup(func() { mgr.Close() })
	return mgr, h
}
