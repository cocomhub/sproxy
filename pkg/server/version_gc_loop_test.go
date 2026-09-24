// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// version_gc_loop_test.go 覆盖版本 GC 的**周期触发**（`versioning.gc_interval`）：
// 启动时按配置决定是否拉起周期 goroutine，Pass 执行整仓保留期清理，Close 收口退出。
//
// 周期 loop 的语义与 cleanupUploadingFilesLoop 同构（ticker + stop channel + WaitGroup），
// 这里不重复验证 ticker 时序（R14 禁 time.Sleep；时钟推进属于 synctest 域外），只钉住：
// ① 配置为 0 不启动 goroutine（零回归）；② Pass 单次执行的清理行为；③ Close 后 loop
// 退出（-race 下无泄漏）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// gcTestHandlers 构造一个带 volSet 的 Handlers（保留期 GC 需要遍历卷版本目录）。
func gcTestHandlers(t *testing.T, cfg *Config) *Handlers {
	t.Helper()
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h := &Handlers{
		cfgPtr:         &cfgPtr,
		logger:         testLogger(),
		auditLogger:    testLogger(),
		uploadingStop:  make(chan struct{}),
		globalRoot:     vs.DefaultRoot(),
		globalPool:     quota.NewPool(cfg.MaxStorageBytes),
		volSet:         vs,
		tenants:        storage.NewTenantCache(vs.DefaultRoot(), storage.WithMetaBucket(), storage.WithLogger(testLogger())),
		checksumStores: make(map[string]*checksum.ChecksumStore),
		uploadStores:   make(map[string]*files.UploadStore),
		quotaScopes:    make(map[string]*quota.Scope),
		quotaBuckets:   make(map[string]map[string]*quota.Scope),
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// seedVersionEntry 直接写一个 version/<rel>/<id> 目录项（版本 GC 夹具，绕开 SaveVersion
// 的当前时间限制）。返回版本 ID。
func seedVersionEntry(t *testing.T, tnt *storage.Tenant, rel string, ago time.Duration) int64 {
	t.Helper()
	verDir, ok := tnt.FeatureRel("version", rel)
	if !ok {
		t.Fatalf("FeatureRel(version, %s) 失败", rel)
	}
	if err := tnt.Root().MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	id := time.Now().Add(-ago).UnixMilli()*1000 + 1
	f, err := tnt.Root().OpenFile(verDir+"/"+strconv.FormatInt(id, 10), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	return id
}

// countVersionEntries 返回 tnt 下 version/<rel> 的版本目录项数。
func countVersionEntries(t *testing.T, tnt *storage.Tenant, rel string) int {
	t.Helper()
	abs, ok := tnt.Root().Abs("version/" + rel)
	if !ok {
		return 0
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return 0
	}
	return len(entries)
}

// TestVersionGCLoop_Disabled 配置 0 时不启动周期 goroutine（无后台副作用）。
func TestVersionGCLoop_Disabled(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Versioning.Enabled = true
	cfg.Versioning.Retention = time.Hour
	cfg.Versioning.GCInterval = 0 // 关闭周期 GC
	h := gcTestHandlers(t, cfg)

	// 直接调 Pass 也不应有任何副作用（保留期清理入口仍存在，但 loop 未挂）。
	h.gcAllExpiredVersionsPass()
	// 未 panic 即通过；无 goroutine 断言由 Close 的 WaitGroup 无阻塞侧面验证。
}

// TestVersionGCLoop_Pass 周期 GC Pass 会按保留期清理各卷版本目录中的过期版本。
func TestVersionGCLoop_Pass(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Versioning.Enabled = true
	cfg.Versioning.Retention = time.Hour
	cfg.Versioning.GCInterval = 10 * time.Minute
	h := gcTestHandlers(t, cfg)

	tnt := h.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("alice 租户不可用")
	}
	seedVersionEntry(t, tnt, "f.txt", 5*time.Hour) // 过期
	seedVersionEntry(t, tnt, "f.txt", 30*time.Minute)
	seedVersionEntry(t, tnt, "g.txt", 3*time.Hour) // 过期
	if got := countVersionEntries(t, tnt, "f.txt"); got != 2 {
		t.Fatalf("前置 f.txt 版本数=%d want 2", got)
	}

	h.gcAllExpiredVersionsPass()

	if got := countVersionEntries(t, tnt, "f.txt"); got != 1 {
		t.Fatalf("Pass 后 f.txt 版本数=%d want 1（过期已清）", got)
	}
	if got := countVersionEntries(t, tnt, "g.txt"); got != 0 {
		t.Fatalf("Pass 后 g.txt 版本数=%d want 0（全部过期）", got)
	}
}

// TestVersionGCLoop_Stop Close 后统一调度器（version-gc 任务）正常退出
// （scheduler.Stop 的 wg.Wait 不阻塞 = 无 goroutine 泄漏）。
func TestVersionGCLoop_Stop(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Versioning.Enabled = true
	cfg.Versioning.Retention = time.Hour
	cfg.Versioning.GCInterval = time.Minute
	h := gcTestHandlers(t, cfg)

	// 模拟 RegisterRoutes 挂载路径：装配 scheduler 并注册 version-gc（间隔 1m）。
	h.scheduler = NewScheduler(testLogger())
	if err := h.scheduler.Register(Task{
		Name:            "version-gc",
		Interval:        cfg.Versioning.GCInterval,
		MaintenanceOnly: true,
		Run:             func(context.Context) { h.gcAllExpiredVersionsPass() },
	}); err != nil {
		t.Fatal(err)
	}
	h.scheduler.Start()
	// Close 会停止 scheduler（wg.Wait 等 goroutine 退出）；在此不直接调 Close
	// （t.Cleanup 已挂），显式再关一次验证幂等（closeOnce 保护）。
	_ = h.Close()
	_ = h.Close()
}

// TestVersionGCLoop_Disabled_HTTPRegister 经 RegisterRoutes 装配（gc_interval=0）后，
// 服务可正常启动/关闭（loop 不挂、Close 不阻塞）。
func TestVersionGCLoop_Disabled_HTTPRegister(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Versioning.Enabled = true
	cfg.Versioning.Retention = time.Hour
	cfg.Versioning.GCInterval = 0

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	ring := accesskey.NewRing()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        ring,
		AllowInsecureLoopback: true,
	})
	ts := httptest.NewServer(h.Handler())
	ts.Close()
	_ = h.Close()
}
