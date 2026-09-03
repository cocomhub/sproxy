// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_reconcile_test.go 验证 P4 任务 17 启动对账：Scope 是纯内存账本，重启后磁盘既有
// 文件占用不计入 Scope → 租户上限与全局 MaxStorageBytes 在上传路径不生效（欠计/失守）。
// ScanAndRecalculate + reconcileQuotaScopes 把磁盘按租户桶归集校准进对应 Scope（Adjust）。

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestScanAndRecalculate_ReconcilesTenantScopes(t *testing.T) {
	env := newOwnerEnv(t)

	// 模拟"重启后"磁盘既有占用（Scope 内存账本为 0）：
	// alice: user 60 + cloud 40；bob: user 20。
	mustWriteFile(t, filepath.Join(env.root, "alice", "user", "f.txt"), 60)
	mustWriteFile(t, filepath.Join(env.root, "alice", "cloud", "t1", "c.bin"), 40)
	mustWriteFile(t, filepath.Join(env.root, "bob", "user", "b.txt"), 20)

	sm := NewStorageManager(env.root, 1024*1024, nil, testLogger())
	sm.SetReconciler(env.h.reconcileQuotaScopes)
	if err := sm.ScanAndRecalculate(); err != nil {
		t.Fatalf("ScanAndRecalculate: %v", err)
	}

	// alice Scope 校准到磁盘总占用（user+cloud）。
	if got := env.h.quotaFor("alice").Usage(); got != 100 {
		t.Fatalf("alice Scope Usage()=%d want 100（重启后不回溯）", got)
	}
	m := env.h.quotaFor("alice").UsageByBucket()
	if got := m["/tenant/alice/user"]; got != 60 {
		t.Fatalf("alice user 桶 = %d want 60", got)
	}
	if got := m["/tenant/alice/cloud"]; got != 40 {
		t.Fatalf("alice cloud 桶 = %d want 40", got)
	}
	// bob Scope 校准。
	if got := env.h.quotaFor("bob").Usage(); got != 20 {
		t.Fatalf("bob Scope Usage()=%d want 20", got)
	}
	// globalPool 聚合 = 全部租户占用（父链聚合）。
	if got := env.h.globalPool.Usage(); got != 120 {
		t.Fatalf("globalPool Usage()=%d want 120（父链聚合）", got)
	}
}

func TestScanAndRecalculate_SkipsInFlightReservation(t *testing.T) {
	env := newOwnerEnv(t)

	// 磁盘已有 alice cloud partial 30；Scope 上有在途预留 100（未 Commit，如云任务下载中）。
	mustWriteFile(t, filepath.Join(env.root, "alice", "cloud", "t1", "c.bin.partial"), 30)
	scope := env.h.quotaBucketFor("alice", "cloud")
	if scope == nil {
		t.Fatal("quotaBucketFor(alice, cloud)=nil")
	}
	rr, err := scope.TryReserve(100)
	if err != nil {
		t.Fatal(err)
	}
	// 未 Commit：reserved=100，committed=0。

	sm := NewStorageManager(env.root, 1024*1024, nil, testLogger())
	sm.SetReconciler(env.h.reconcileQuotaScopes)
	if err := sm.ScanAndRecalculate(); err != nil {
		t.Fatalf("ScanAndRecalculate: %v", err)
	}

	// 在途预留桶跳过校准：committed 仍为 0（不把 reserved 的 partial 双计）。
	if got := scope.Usage(); got != 0 {
		t.Fatalf("cloud 桶 committed=%d want 0（在途预留跳过校准）", got)
	}
	// 预留仍在（Commit 后可正常对账到磁盘实际）。
	rr.Release()
}

// TestRegisterRoutes_StartupReconcilesQuotaScopes 验证 RegisterRoutes 启动装配后：
// 磁盘既有占用（重启前已落盘）按租户桶校准进 per-tenant Scope（重启后 Scope 不回溯），
// 且 StorageManager 扫描的是 storage_root（而非 storage_root，二者在 storage_root 配置下不同）。
func TestRegisterRoutes_StartupReconcilesQuotaScopes(t *testing.T) {
	storageRoot := t.TempDir()
	// 预置磁盘占用：anonymous/user/a.txt 50 + anonymous/cloud/t1/c.bin 30。
	mustWriteFile(t, filepath.Join(storageRoot, "anonymous", "user", "a.txt"), 50)
	mustWriteFile(t, filepath.Join(storageRoot, "anonymous", "cloud", "t1", "c.bin"), 30)

	cfg := Default()
	cfg.StorageRoot = storageRoot
	cfg.MaxStorageBytes = 1024 * 1024
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "t", BuildAt: "t",
		Logger: testLogger(), AuditLogger: testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	if got := h.quotaFor("anonymous").Usage(); got != 80 {
		t.Fatalf("启动对账后 anonymous Usage()=%d want 80（重启不回溯）", got)
	}
	m := h.quotaFor("anonymous").UsageByBucket()
	if got := m["/tenant/anonymous/user"]; got != 50 {
		t.Fatalf("anonymous user 桶 = %d want 50", got)
	}
	if got := m["/tenant/anonymous/cloud"]; got != 30 {
		t.Fatalf("anonymous cloud 桶 = %d want 30", got)
	}
	if got := h.globalPool.Usage(); got != 80 {
		t.Fatalf("globalPool Usage()=%d want 80（父链聚合）", got)
	}
}

// TestRegisterRoutes_StartupReconcilesQuotaScopes_WithBucketLimits 验证（审查 D）startup
// 装配 bucket_limits 回归：
//  1. RegisterRoutes 装配后按 BucketLimits 建精确路径子 Scope（quotaBucketFor 命中、
//     MaxBytes=100），不因子目录未发生任何写而缺失；
//  2. 写路径超子目录上限（user/videos/hd 100B）但满足租户总上限（OwnerQuotas 200）→ 200
//     （写路径仍按 user 桶聚合，不被 bucket_limits 子 Scope 截断）；
//  3. 写路径超租户总上限 → 507（owner 兜底仍生效），且 user 桶不泄漏。
func TestRegisterRoutes_StartupReconcilesQuotaScopes_WithBucketLimits(t *testing.T) {
	storageRoot := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = storageRoot
	cfg.MaxStorageBytes = 1024 * 1024
	cfg.OwnerQuotas = map[string]int64{"anonymous": 200}
	cfg.BucketLimits = map[string]int64{"user/videos/hd": 100}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "t", BuildAt: "t",
		Logger: testLogger(), AuditLogger: testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	// 1) bucket_limits 精确路径子 Scope 已装配（startup 回归）：非 nil、上限 100。
	subScope := h.quotaBucketFor("anonymous", "user/videos/hd")
	if subScope == nil {
		t.Fatal("RegisterRoutes 装配后 quotaBucketFor(anonymous, user/videos/hd)=nil（bucket_limits 未装配？）")
	}
	if got := subScope.MaxBytes(); got != 100 {
		t.Fatalf("bucket_limits 子目录 Scope MaxBytes=%d want 100", got)
	}

	// 写路径 mux（注入 actor）。
	umux := actorUploadDeleteMux(h, "anonymous")

	// 2) 上传 140 字节（>子目录上限 100，但 140<租户总上限 200）→ 200（写路径不截断）。
	code, resp := uploadAsPath(t, umux, "user/videos/hd/big.txt", bytes.Repeat([]byte("a"), 140))
	if code != http.StatusOK {
		t.Fatalf("超子目录上限但满足租户总上限应 200, got %d: %s", code, resp)
	}
	if got := h.quotaBucketFor("anonymous", "user").Usage(); got != 140 {
		t.Fatalf("上传 140 后 user 桶 Usage()=%d want 140", got)
	}

	// 3) 再上传 140 字节 → user 桶累计 280 > 租户总上限 200 → 507（owner 兜底仍生效）。
	code2, resp2 := uploadAsPath(t, umux, "user/videos/hd/big2.txt", bytes.Repeat([]byte("b"), 140))
	if code2 != http.StatusInsufficientStorage {
		t.Fatalf("累计超租户总上限应 507, got %d: %s", code2, resp2)
	}
	if got := h.quotaBucketFor("anonymous", "user").Usage(); got != 140 {
		t.Fatalf("507 后 user 桶 Usage()=%d want 140（不泄漏）", got)
	}
	if got := h.quotaBucketFor("anonymous", "user").Reserved(); got != 0 {
		t.Fatalf("507 后 user 桶 Reserved()=%d want 0（不泄漏）", got)
	}
	// 写路径不接入子 Scope：子目录子 Scope 始终未记账（0）。
	if got := subScope.Usage(); got != 0 {
		t.Fatalf("写路径不应接入子目录子 Scope（Usage()=%d want 0）", got)
	}
}

// mustWriteFile 写 size 字节的文件到 path（自动建目录）。
func mustWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
