// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// assembly_adapters_test.go 钉住**装配层薄适配与访问器**的行为（此前零覆盖）：
//   - Handlers 只读访问器 / setter（装配层与 xfer listener 的唯一取用入口）；
//   - sync 配额适配器（syncmgr.QuotaStore 的两个实现）与租户解析注入点；
//   - backupQuotaFS 的三本账透传（scope/pool/持久账本）+ backupReservation 的
//     预留/对账/回退语义。

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestHandlers_AssemblyAccessors 钉住装配层取用入口：setter 写入后 getter 必须返回同一实例
// （装配层据此把同一实例注入多个子系统，如 AI 洞察器与事件消费端）。
func TestHandlers_AssemblyAccessors(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	if h.AlertEngine() != nil {
		t.Fatal("未注入 alertEngine 时应返回 nil")
	}
	if h.AIInsight() != nil {
		t.Fatal("未注入 aiInsight 时应返回 nil")
	}
	ai := NewAIInsightWithGate(nil)
	h.SetAIInsight(ai)
	if h.AIInsight() != ai {
		t.Fatal("SetAIInsight 后 AIInsight 应返回同一实例")
	}
	priv := NewAIPrivacy(false, nil, testLogger())
	h.SetAIPrivacy(priv)
	if h.aiPrivacy != priv {
		t.Fatal("SetAIPrivacy 应写入 aiPrivacy")
	}
	nr := NewNodeRegistry(nil, testLogger())
	h.SetNodeRegistry(nr)
	if h.nodeRegistry != nr {
		t.Fatal("SetNodeRegistry 应写入 nodeRegistry")
	}
	if h.Metrics() == nil {
		t.Fatal("buildVolSetHandlers 已装配 metrics，Metrics() 不应为 nil")
	}
	if h.EventsBus() == nil {
		t.Fatal("EventsBus 应懒建（非 nil）")
	}
	if h.FileService() == nil {
		t.Fatal("FileService 应懒装配（非 nil）")
	}
	if h.PrivacyRoot() == nil {
		t.Fatal("已装配 tenants 时 PrivacyRoot 应非 nil")
	}
	if dir := h.InsightCacheDir(); dir == "" {
		t.Fatal("已装配 tenants 时 InsightCacheDir 应非空")
	}
	// SetVectorStore 转发给文件服务域（未装配向量索引 ⇒ 只要求不 panic 且服务已就绪）。
	h.SetVectorStore(nil)
	if h.FileService() == nil {
		t.Fatal("SetVectorStore 后 FileService 仍应可用")
	}
	if h.AIEventConsumer() != nil {
		t.Fatal("未注入 aiEventConsumer 时应返回 nil")
	}
	h.SetAIEventConsumer(nil)
	if BackgroundContext() == nil {
		t.Fatal("BackgroundContext 不应返回 nil")
	}

	// 未装配 tenants：两个卷根访问器必须 fail-closed 返回零值（不 panic、不回落全局根）。
	empty := &Handlers{}
	if empty.PrivacyRoot() != nil {
		t.Fatal("未装配 tenants 时 PrivacyRoot 应为 nil")
	}
	if empty.InsightCacheDir() != "" {
		t.Fatal("未装配 tenants 时 InsightCacheDir 应为空")
	}
}

// TestHandlers_DeniedTenantQuota 钉住超售 fail-closed 载体：上限 1 字节 ⇒ 任何非零预留失败，
// 建树本身不因 I1 失败（所有节点上限 0 = 不限制）。
func TestHandlers_DeniedTenantQuota(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	s, buckets := h.deniedTenantQuota()
	if s == nil || len(buckets) != len(quotaBucketNames) {
		t.Fatalf("拒写配额树装配不完整: scope=%v buckets=%d", s, len(buckets))
	}
	if _, err := s.TryReserve(2); err == nil {
		t.Fatal("上限 1 字节的拒写树必须拒绝 2 字节预留")
	}
	if _, err := s.TryReserve(1); err != nil {
		t.Fatalf("1 字节预留应成功（上限 1）: %v", err)
	}
	if buckets["user"] == nil {
		t.Fatal("拒写树必须挂载 user 桶")
	}
}

// TestHandlers_StagingQuotaTrackerFor 钉住导出适配：同一 owner 复用同一 tracker（跨请求
// ReleaseUsage 广播才能唤醒等待者），未装配 globalPool ⇒ nil（零回归）。
func TestHandlers_StagingQuotaTrackerFor(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	tr := h.StagingQuotaTrackerFor("alice")
	if tr == nil {
		t.Fatal("已装配 globalPool 时 StagingQuotaTrackerFor 应非 nil")
	}
	if again := h.StagingQuotaTrackerFor("alice"); again != tr {
		t.Fatal("同一 owner 必须复用同一 staging tracker")
	}
	if other := h.StagingQuotaTrackerFor("bob"); other == tr {
		t.Fatal("不同 owner 必须使用独立 tracker")
	}

	none := &Handlers{}
	if got := none.StagingQuotaTrackerFor("alice"); got != nil {
		t.Fatal("未装配 globalPool 时应返回 nil")
	}
}

// TestScopeQuotaAdapter_SingleCounter 钉住 per-owner Scope 适配为单计数器语义：
// TryReserve 立即 Commit（预留转已用）、Release 扣减、Usage/MaxBytes 与 Scope 同源。
func TestScopeQuotaAdapter_SingleCounter(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	sc := pool.Scope("/tenant/alice", 100)
	a := scopeQuotaAdapter{scope: sc}
	if err := a.TryReserve(10, 0); err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	if got := a.Usage(); got != 10 {
		t.Fatalf("Usage=%d want 10（预留应已 Commit）", got)
	}
	if got := a.MaxBytes(); got != 100 {
		t.Fatalf("MaxBytes=%d want 100", got)
	}
	a.Release(10, 0)
	if got := a.Usage(); got != 0 {
		t.Fatalf("Release 后 Usage=%d want 0", got)
	}
	if err := a.TryReserve(101, 0); err == nil {
		t.Fatal("超过 Scope 上限的预留必须失败")
	}
}

// TestSyncQuotaAdapter_GlobalLedger 钉住全局账本回退适配（globalPool 未装配时的旧行为）。
func TestSyncQuotaAdapter_GlobalLedger(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 100, nil, testLogger())
	t.Cleanup(sm.Stop)
	a := syncQuotaAdapter{sm: sm}
	if err := a.TryReserve(10, int(capacity.CategoryCloud)); err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	if got := a.Usage(); got != 10 {
		t.Fatalf("Usage=%d want 10", got)
	}
	if got := a.MaxBytes(); got != 100 {
		t.Fatalf("MaxBytes=%d want 100", got)
	}
	a.Release(10, int(capacity.CategoryCloud))
	if got := a.Usage(); got != 0 {
		t.Fatalf("Release 后 Usage=%d want 0", got)
	}
}

// TestHandlers_SyncWiringAdapters 钉住 syncmgr 注入点：配额解析器按 owner 路由到 user 桶
// Scope（未装配时回退全局账本）、租户根/物理口径解析器、租户列表来源。
func TestHandlers_SyncWiringAdapters(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	cfg.OwnerQuotas = map[string]ByteSize{"alice": ByteSize(1000)}
	h := buildVolSetHandlers(t, cfg)

	store := h.SyncQuotaStore()
	if store == nil {
		t.Fatal("SyncQuotaStore 不应为 nil")
	}
	if got := store("alice"); got == nil {
		t.Fatal("alice 的配额存储不应为 nil")
	} else if _, ok := got.(scopeQuotaAdapter); !ok {
		t.Fatalf("已装配配额时应返回 scopeQuotaAdapter, got %T", got)
	}
	if sc := h.SyncQuotaScope()("alice"); sc == nil {
		t.Fatal("SyncQuotaScope 应解析出 user 桶 Scope")
	}
	if sc := h.SyncScopeFor()("alice", "user/a.bin"); sc == nil {
		t.Fatal("SyncScopeFor 应解析出文件级 Scope")
	}
	if fsys, ok := h.SyncTenantRootFS("alice"); !ok || fsys == nil {
		t.Fatal("SyncTenantRootFS 应返回租户根 FS")
	}
	// 明文卷：记账口径恒等 ⇒ 返回 nil（不折算），与 pkg/files.RootPhysSizer 同源。
	if phys := h.SyncTenantPhysSize("alice"); phys != nil {
		t.Fatalf("明文卷 SyncTenantPhysSize 应为 nil（恒等）, got %v", phys(7))
	}
	resolver := h.SyncTenantResolver()
	if resolver == nil {
		t.Fatal("SyncTenantResolver 不应为 nil")
	}
	userRoot, metaDir, ok := resolver("alice")
	if !ok || userRoot == "" || metaDir == "" {
		t.Fatalf("租户根解析失败: (%q,%q,%v)", userRoot, metaDir, ok)
	}
	if list := h.SyncTenantList(); list == nil || len(list()) == 0 {
		t.Fatal("SyncTenantList 应返回租户名列表（含 anonymous）")
	}

	// 未装配：fail-closed（nil / ok=false），绝不回落全局根。
	empty := &Handlers{}
	if _, ok := empty.SyncTenantRootFS("alice"); ok {
		t.Fatal("未装配 tenants 时 SyncTenantRootFS 必须 ok=false")
	}
	if empty.SyncTenantPhysSize("alice") != nil {
		t.Fatal("未装配 tenants 时 SyncTenantPhysSize 必须为 nil")
	}
	if got := empty.SyncQuotaStore()("alice"); got == nil {
		t.Fatal("未装配配额时 SyncQuotaStore 应回退全局账本适配器（非 nil）")
	}
}

// backupQuotaFSFixture 构造带三本账的备份写面（inner = 内存 FS）。
func backupQuotaFSFixture(t *testing.T) (*backupQuotaFS, *quota.Pool, *capacity.StorageManager, syncpkg.FS) {
	t.Helper()
	// Scope 与卷池**必须是两棵独立的池**（Scope 会向父链聚合；挂在卷池下会双计）。
	tenantPool := quota.NewPool(1000)
	pool := quota.NewPool(1000)
	sm := capacity.NewStorageManager(t.TempDir(), 1000, nil, testLogger())
	t.Cleanup(sm.Stop)
	inner := syncpkg.NewLocalFS(t.TempDir(), testLogger())
	q := &backupQuotaFS{
		inner:       inner,
		scope:       tenantPool.Scope("/tenant/alice", 1000),
		pool:        pool,
		sm:          sm,
		cat:         capacity.CategoryChunked,
		log:         testLogger(),
		waitTimeout: 50_000_000, // 50ms：容量不足时不白等 30s
	}
	return q, pool, sm, inner
}

// TestBackupQuotaFS_PassthroughAndLedger 钉住备份写面装饰器：只读/目录方法透传 inner，
// 写入在 scope/pool/持久账本三处记账，删除与覆盖改名释放旧字节（幂等：缺失不报错）。
func TestBackupQuotaFS_PassthroughAndLedger(t *testing.T) {
	t.Parallel()
	q, pool, sm, inner := backupQuotaFSFixture(t)
	ctx := context.Background()

	if err := q.MakeDir(ctx, "dir"); err != nil {
		t.Fatalf("MakeDir 应透传: %v", err)
	}
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "dir/a.bin", bytes.NewReader(bytes.Repeat([]byte("x"), 40)), 40, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := pool.Usage(); got != 40 {
		t.Fatalf("卷池 Usage=%d want 40（写成功应 Commit）", got)
	}
	if got := sm.Usage(); got != 40 {
		t.Fatalf("持久账本 Usage=%d want 40", got)
	}
	if e, err := q.Stat(ctx, "dir/a.bin"); err != nil || e == nil || e.Size != 40 {
		t.Fatalf("Stat 应透传 inner: e=%v err=%v", e, err)
	}
	if list, err := q.ListDir(ctx, "dir"); err != nil || len(list) != 1 {
		t.Fatalf("ListDir 应透传 inner: %v %v", list, err)
	}
	rc, err := q.OpenRead(ctx, "dir/a.bin")
	if err != nil {
		t.Fatalf("OpenRead 应透传 inner: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || len(data) != 40 {
		t.Fatalf("OpenRead 内容异常: %d %v", len(data), err)
	}
	if q.Inner() != syncpkg.FS(inner) {
		t.Fatal("Inner 应返回被包装 FS（staging 能力判定依赖）")
	}
	// WithStagingQuota 转发到内层：inner 未实现 StagingQuotaCapable ⇒ 空操作（不 panic）。
	q.WithStagingQuota(nil)

	// 覆盖改名：目标旧字节释放（源字节已在写入时计入，改名不重计）。
	if err := q.WriteFile(ctx, syncpkg.LogicalKey{}, "dir/b.bin", bytes.NewReader(bytes.Repeat([]byte("y"), 10)), 10, 0); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}
	if got := pool.Usage(); got != 50 {
		t.Fatalf("写两文件后 Usage=%d want 50", got)
	}
	if err := q.Rename(ctx, "dir/a.bin", "dir/b.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := pool.Usage(); got != 40 {
		t.Fatalf("覆盖改名后 Usage=%d want 40（旧目标 10 字节应释放）", got)
	}
	if err := q.Delete(ctx, "dir/b.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := pool.Usage(); got != 0 {
		t.Fatalf("删除后 Usage=%d want 0", got)
	}
	// 删除不存在的键：inner 返回 not-exist ⇒ 错误透传且不记账。
	if err := q.Delete(ctx, "dir/missing.bin"); err == nil {
		t.Fatal("删除不存在的键应返回 inner 错误")
	}
}

// TestBackupReservation_ReleaseAndSettle 钉住三预留聚合的释放/对账语义：
// Release 归还三本账；settleSM 走「Release(pre)+TryReserve(actual)」；写失败回退预留。
func TestBackupReservation_ReleaseAndSettle(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1000)
	sm := capacity.NewStorageManager(t.TempDir(), 1000, nil, testLogger())
	t.Cleanup(sm.Stop)
	sc := pool.Scope("/tenant/alice", 1000)

	ctx := context.Background()

	// ① Commit：预留转已确认占用，持久账本按实际大小对账（30 → 20 ⇒ 释放 10）。
	res, err := sc.WaitReserve(ctx, 30, quota.DefaultWaitTimeout)
	if err != nil {
		t.Fatalf("WaitReserve: %v", err)
	}
	if serr := sm.TryReserve(30, capacity.CategoryChunked); serr != nil {
		t.Fatalf("sm.TryReserve: %v", serr)
	}
	r := &backupReservation{scope: res, sm: sm, cat: capacity.CategoryChunked, log: testLogger(), smPre: 30}
	r.Commit(20)
	if got := sm.Usage(); got != 20 {
		t.Fatalf("Commit 后持久账本 Usage=%d want 20", got)
	}
	if got := sc.Usage(); got != 20 {
		t.Fatalf("Commit 后 Scope Usage=%d want 20", got)
	}

	// ② Release（写失败/取消）：归还 scope + 持久账本预留。
	res2, err := sc.WaitReserve(ctx, 30, quota.DefaultWaitTimeout)
	if err != nil {
		t.Fatalf("WaitReserve: %v", err)
	}
	if err := sm.TryReserve(30, capacity.CategoryChunked); err != nil {
		t.Fatalf("sm.TryReserve: %v", err)
	}
	fail := &backupReservation{scope: res2, sm: sm, cat: capacity.CategoryChunked, log: testLogger(), smPre: 30}
	fail.Release()
	if got := sc.Usage(); got != 20 {
		t.Fatalf("Release 后 Scope Usage=%d want 20（仅归还本次预留）", got)
	}
	if got := sm.Usage(); got != 20 {
		t.Fatalf("Release 后持久账本 Usage=%d want 20", got)
	}
	fail.Release() // 一次性（已 done）⇒ 不重复扣减
	if got := sm.Usage(); got != 20 {
		t.Fatalf("重复 Release 不应重复扣减, Usage=%d", got)
	}

	// ③ releaseSM 回退持久账本预留。
	if err := sm.TryReserve(15, capacity.CategoryChunked); err != nil {
		t.Fatalf("sm.TryReserve: %v", err)
	}
	back := &backupReservation{sm: sm, cat: capacity.CategoryChunked, smPre: 15}
	back.releaseSM()
	if got := sm.Usage(); got != 20 {
		t.Fatalf("releaseSM 后 Usage=%d want 20", got)
	}
	back.releaseSM() // 已清零 ⇒ 无操作
}

// TestBackupReservation_ReserveMemoryRollback 钉住「卷池预留失败 ⇒ 回退已成功的 Scope」：
// 否则一次失败请求会永久占用 owner 配额（泄漏）。
func TestBackupReservation_ReserveMemoryRollback(t *testing.T) {
	t.Parallel()
	scopePool := quota.NewPool(1000)
	sc := scopePool.Scope("/tenant/alice", 1000)
	tinyPool := quota.NewPool(10) // 不足 20 ⇒ 卷池预留失败
	r := &backupReservation{}
	err := r.reserveMemory(context.Background(), 20, sc, tinyPool, 50_000_000)
	if err == nil {
		t.Fatal("卷池不足时 reserveMemory 应返回错误")
	}
	if got := sc.Usage(); got != 0 {
		t.Fatalf("回退后 Scope Usage=%d want 0（预留泄漏）", got)
	}
	if r.scope != nil {
		t.Fatal("回退后 reservation.scope 应为 nil")
	}
}
