// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// newCloudTestManager 创建 CloudDownloadManager，装配基于 storageRoot 的租户解析闭包。
// 复用 newAssemblyTestHandlers 提供的 tenantFor/checksumStoreFor/listTenantIDs，
// 使测试无需手工构造 TenantResolver。返回 (manager, 配套 Handlers)；Handlers 仅供测试
// 读取 per-tenant checksum store / 验证租户根等。
func newCloudTestManager(t *testing.T, storageRoot string, sm *StorageManager, cfg *CloudDownloadConfig) (*CloudDownloadManager, *Handlers) {
	t.Helper()
	h := newAssemblyTestHandlers(t, storageRoot)
	mgr := NewCloudDownloadManager(storageRoot, sm, h.tenantFor, h.checksumStoreFor, h.listTenantIDs, testLogger(), cfg, func(owner string) *quota.Scope {
		return h.quotaBucketFor(owner, "cloud")
	})
	h.cloudMgr = mgr
	t.Cleanup(func() { mgr.Close() })
	return mgr, h
}
