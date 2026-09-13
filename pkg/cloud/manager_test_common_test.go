// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_test_common_test.go 提供 pkg/cloud 域级用例的测试基座。
//
// 设计取法：本包的用例**只驱动领域对象**（`*CloudDownloadManager`），不经过装配层的
// HTTP 路由与 `Handlers`。故需要一个最小的 `cloudTestEnv`：它实现域声明的三个窄函数
// 类型（`TenantResolver`/`ChecksumResolver`/`QuotaResolver`）+ 租户扫描，并复刻装配层
// 中与云任务相关的那部分配额装配（租户 Scope + 功能桶子 Scope）。
//
// 边界（刻意不复制的东西）：
//   - 不做 HTTP 路由 / 认证（那是装配层的断言对象，相关用例留在 pkg/server）；
//   - 不复制 `bucket_limits` 的段树装配（云任务的配额只走功能桶根，本环境按同一挂载
//     语义建 5 个功能桶根 Scope，上限 0 = 不限量，与装配层 `ensureTenantQuotaLocked`
//     的默认形态一致）；
//   - `setOwnerQuota` 对应装配层的 `setTestOwnerQuota`（改 owner 配额后由 `quotaFor`
//     懒建的 Scope 上限生效——与本包用例的使用方式一致）。
package cloud

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// testLogger 返回静默的测试 logger（与 pkg/server 的 testLogger 同款：只放行 Error）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// quotaBucketNames 是参与配额归集的功能桶名（与装配层的同名白名单一致）。
//
// 测试**允许**导入子包 `pkg/storage/capacity`：门禁 R2 只约束生产代码（archcheck 解析
// `go list` 的非测试导入集），这正是本环境能构造真实 `*capacity.StorageManager` 的原因。
var quotaBucketNames = []string{"user", "cloud", "archive", "chunk", "version", "meta"}

// cloudTestEnv 是域级用例的最小环境：真实租户缓存 + 真实 checksum 台账 + 真实配额 Scope。
type cloudTestEnv struct {
	root       string
	tenants    *storage.TenantCache
	globalPool *quota.Pool

	mu             sync.Mutex
	checksumStores map[string]*checksum.ChecksumStore
	quotaScopes    map[string]*quota.Scope
	quotaBuckets   map[string]map[string]*quota.Scope
	ownerQuotas    map[string]int64
}

// newCloudTestEnv 在 storageRoot 下装配最小环境。
func newCloudTestEnv(t *testing.T, storageRoot string) *cloudTestEnv {
	t.Helper()
	root, err := storage.OpenRoot(storageRoot)
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tenants := storage.NewTenantCache(root, storage.WithMetaBucket(), storage.WithLogger(testLogger()))
	// LIFO：本清理晚于 root.Close 注册 ⇒ 先关各租户子根，再关存储根
	// （Windows 上遗留打开的目录句柄会让 TempDir 清理失败）。
	t.Cleanup(func() { _ = tenants.Close() })
	return &cloudTestEnv{
		root:           storageRoot,
		tenants:        tenants,
		globalPool:     quota.NewPool(0),
		checksumStores: map[string]*checksum.ChecksumStore{},
		quotaScopes:    map[string]*quota.Scope{},
		quotaBuckets:   map[string]map[string]*quota.Scope{},
		ownerQuotas:    map[string]int64{},
	}
}

// tenantFor 实现 TenantResolver（空 owner 归一为 anonymous，与领域约定一致）。
func (e *cloudTestEnv) tenantFor(owner string) *storage.Tenant {
	return e.tenants.TenantFor(storage.NormalizeOwner(owner))
}

// checksumStoreFor 实现 ChecksumResolver：per-tenant meta 桶下的 checksums.json。
func (e *cloudTestEnv) checksumStoreFor(owner string) *checksum.ChecksumStore {
	owner = storage.NormalizeOwner(owner)
	e.mu.Lock()
	defer e.mu.Unlock()
	if cs, ok := e.checksumStores[owner]; ok {
		return cs
	}
	tnt := e.tenants.TenantFor(owner)
	if tnt == nil {
		return nil
	}
	abs, ok := tnt.Root().Abs("meta")
	if !ok {
		return nil
	}
	cs := checksum.NewChecksumStore(filepath.Join(abs, "checksums.json"), testLogger())
	e.checksumStores[owner] = cs
	return cs
}

// listTenantIDs 实现租户扫描（重启恢复用例使用）。
func (e *cloudTestEnv) listTenantIDs() []string {
	root, err := storage.OpenRoot(e.root)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	return storage.ListOwners(root)
}

// setOwnerQuota 设置 owner 的租户配额上限（等价于装配层测试的 setTestOwnerQuota）。
// 仅对**之后**懒创建的 Scope 生效（与既有语义一致：Scope 一旦创建即固化上限）。
func (e *cloudTestEnv) setOwnerQuota(owner string, bytes int64) {
	e.mu.Lock()
	e.ownerQuotas[owner] = bytes
	e.mu.Unlock()
}

// ensureTenantQuota 懒建 owner 的租户 Scope 与功能桶子 Scope（复刻装配层语义的简化版）。
func (e *cloudTestEnv) ensureTenantQuota(owner string) (*quota.Scope, map[string]*quota.Scope) {
	if s, ok := e.quotaScopes[owner]; ok {
		return s, e.quotaBuckets[owner]
	}
	s := e.globalPool.Scope("/tenant/"+owner, e.ownerQuotas[owner])
	buckets := make(map[string]*quota.Scope, len(quotaBucketNames))
	for _, b := range quotaBucketNames {
		buckets[b] = s.Mount(b, 0)
	}
	e.quotaScopes[owner] = s
	e.quotaBuckets[owner] = buckets
	return s, buckets
}

// quotaFor 返回 owner 的租户 Scope（等价于装配层 Handlers.quotaFor）。
func (e *cloudTestEnv) quotaFor(owner string) *quota.Scope {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, _ := e.ensureTenantQuota(storage.NormalizeOwner(owner))
	return s
}

// quotaBucketFor 返回 owner 下指定功能桶的 Scope（等价于装配层 Handlers.quotaBucketFor
// 的「单个功能桶名」用法——云任务只消费 cloud 桶）。
func (e *cloudTestEnv) quotaBucketFor(owner, bucket string) *quota.Scope {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, buckets := e.ensureTenantQuota(storage.NormalizeOwner(owner))
	if buckets == nil {
		return nil
	}
	return buckets[bucket]
}

// cloudTestStorageManager 把真实 `*capacity.StorageManager` 适配为领域窄接口
// `StorageManager`。**只在测试中存在**：生产侧的同类适配器在 pkg/server
// （cloudStorageManager）——本包不得导入装配层，故测试自备一份（类别同样固定为 CategoryCloud）。
type cloudTestStorageManager struct{ m *capacity.StorageManager }

func (a cloudTestStorageManager) TryReserveCloud(n int64) error {
	return a.m.TryReserve(n, capacity.CategoryCloud)
}
func (a cloudTestStorageManager) ReleaseCloud(n int64) { a.m.Release(n, capacity.CategoryCloud) }
func (a cloudTestStorageManager) Usage() int64         { return a.m.Usage() }
func (a cloudTestStorageManager) MaxBytes() int64      { return a.m.MaxBytes() }

// newCloudTestManager 创建领域管理器（等价于 pkg/server 的同名测试辅助，但只依赖本环境的
// 窄函数，不经过 Handlers）。签名刻意与装配层版本一致（接收真实 `*capacity.StorageManager`
// 并在此包装），使迁入的用例零改动。返回 (manager, 环境)。
func newCloudTestManager(t *testing.T, storageRoot string, sm *capacity.StorageManager, cfg *CloudDownloadConfig) (*CloudDownloadManager, *cloudTestEnv) {
	t.Helper()
	env := newCloudTestEnv(t, storageRoot)
	var storageCap StorageManager
	if sm != nil {
		storageCap = cloudTestStorageManager{m: sm}
	}
	mgr := NewCloudDownloadManager(storageRoot, storageCap,
		env.tenantFor, env.checksumStoreFor, env.listTenantIDs, testLogger(),
		cfg, func(owner string) *quota.Scope { return env.quotaBucketFor(owner, "cloud") })
	t.Cleanup(func() { mgr.Close() })
	return mgr, env
}
