// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_retention_test.go 覆盖卷级数据保留策略（roadmap 11.7-⑨，volumes[].retention）：
//  1. Config Validate：负值拒绝；gc_interval>0 且全 TTL 为 0 的空转任务拒绝；零值全关闭零回归。
//  2. 龄判定纯函数（now 注入）表驱动：created < now-ttl 过期、== 边界不过期、ttl<=0 未启用。
//  3. 装配透传：volume.Volume.Retention 与配置一致；非默认卷 AuditTTL → Warn + 清零（禁静默忽略）。
//  4. 清理执行器（核心变异点）：fixture 卷放过期/未过期版本 + 分享 token + 审计行，
//     手动触发一次 GC → 过期删除、未过期保留。
//  5. 关闭语义：全零 retention 不启动 goroutine、pass 无副作用（零回归）。

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// retentionTestHandlers 构造带 volSet + 可选 shareStore/auditStore 的 Handlers
// （卷级保留清理执行器测试环境）。
func retentionTestHandlers(t *testing.T, cfg *Config, shareDir string, audit *AuditStore) *Handlers {
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
		globalPool:     nil,
		volSet:         vs,
		tenants:        storage.NewTenantCache(vs.DefaultRoot(), storage.WithMetaBucket(), storage.WithLogger(testLogger())),
		checksumStores: make(map[string]*checksum.ChecksumStore),
		uploadStores:   make(map[string]*files.UploadStore),
		quotaScopes:    make(map[string]*quota.Scope),
		quotaBuckets:   make(map[string]map[string]*quota.Scope),
		shareStore:     NewShareStore(testLogger()),
	}
	if shareDir != "" {
		h.shareStore.EnablePersist(shareDir)
	}
	h.auditStore = audit
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// seedRetentionShare 在 shareStore 创建一条分享并改写 CreatedAt（卷级 TTL 兜底按创建时间
// 起算——直接改内存对象 + 落盘，模拟「创建于过去但 ExpiresAt 未到」的兜底场景）。
func seedRetentionShare(t *testing.T, h *Handlers, filename string, createdAgo time.Duration, ttl time.Duration) string {
	t.Helper()
	link, err := h.shareStore.Create(filename, "alice", "user/"+filename, "ak-1", ttl, 0, false, false, "")
	if err != nil {
		t.Fatalf("Create share: %v", err)
	}
	link.CreatedAt = time.Now().Add(-createdAgo)
	h.shareStore.persistWrite(link)
	return link.Token
}

// countSharePersistFiles 返回分享持久化目录中的 json 文件数。
func countSharePersistFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("ReadDir share dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// TestVolumeRetention_ConfigValidate 配置校验：负值拒绝、空转 GC 拒绝、零值零回归。
func TestVolumeRetention_ConfigValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*VolumeConfig)
		wantErr string
	}{
		{"零值全关闭", func(v *VolumeConfig) {}, ""},
		{"合法版本+GC", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{VersionTTL: 24 * time.Hour, GCInterval: time.Hour}
		}, ""},
		{"合法分享仅手动", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{ShareTTL: time.Hour}
		}, ""},
		{"负版本 TTL", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{VersionTTL: -time.Hour}
		}, "不能为负"},
		{"负分享 TTL", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{ShareTTL: -time.Hour}
		}, "不能为负"},
		{"负审计 TTL", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{AuditTTL: -time.Hour}
		}, "不能为负"},
		{"负 GC 间隔", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{GCInterval: -time.Hour}
		}, "不能为负"},
		{"GC 开启但全 TTL 零（空转）", func(v *VolumeConfig) {
			v.Retention = VolumeRetentionConfig{GCInterval: time.Hour}
		}, "空转"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.StorageRoot = t.TempDir()
			tc.mutate(&c.Volumes[0])
			err := c.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("期望通过, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestVolumeRetention_AgeCutoff 龄判定纯函数表驱动（now 注入，确定性）。
func TestVolumeRetention_AgeCutoff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		created time.Time
		ttl     time.Duration
		want    bool
	}{
		{"未启用 ttl=0", now.Add(-10 * time.Hour), 0, false},
		{"未启用 ttl<0", now.Add(-10 * time.Hour), -time.Hour, false},
		{"窗口内保留", now.Add(-30 * time.Minute), time.Hour, false},
		{"恰好边界不过期", now.Add(-time.Hour), time.Hour, false},
		{"超龄过期", now.Add(-2 * time.Hour), time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeRetentionExpired(tc.created, now, tc.ttl); got != tc.want {
				t.Fatalf("volumeRetentionExpired(created=%v, ttl=%v) = %v, want %v",
					tc.created, tc.ttl, got, tc.want)
			}
		})
	}
}

// TestVolumeRetention_AssemblyPassthrough 装配透传：Retention 落入 volume.Volume；
// 非默认卷 AuditTTL 清零 + Warn。
func TestVolumeRetention_AssemblyPassthrough(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir()}
	var warnBuf bytes.Buffer
	warnLog := slog.New(slog.NewTextHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0], Retention: VolumeRetentionConfig{
			VersionTTL: 24 * time.Hour, ShareTTL: 2 * time.Hour, AuditTTL: 3 * time.Hour, GCInterval: time.Hour,
		}},
		{Name: "disk2", Root: dirs[1], Retention: VolumeRetentionConfig{
			VersionTTL: 48 * time.Hour, AuditTTL: time.Hour, // 非默认卷 AuditTTL → 忽略 + Warn
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	vs, err := assembleVolumes(cfg, warnLog)
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	main, ok := vs.ByName("main")
	if !ok {
		t.Fatal("main 卷缺失")
	}
	if main.Retention != (volume.Retention{
		VersionTTL: 24 * time.Hour, ShareTTL: 2 * time.Hour, AuditTTL: 3 * time.Hour, GCInterval: time.Hour,
	}) {
		t.Fatalf("main Retention 透传不符: %+v", main.Retention)
	}
	disk2, ok := vs.ByName("disk2")
	if !ok {
		t.Fatal("disk2 卷缺失")
	}
	if disk2.Retention.VersionTTL != 48*time.Hour || disk2.Retention.AuditTTL != 0 {
		t.Fatalf("disk2 非默认卷 AuditTTL 应清零（VersionTTL=%v AuditTTL=%v）",
			disk2.Retention.VersionTTL, disk2.Retention.AuditTTL)
	}
	if !strings.Contains(warnBuf.String(), "audit_ttl") || !strings.Contains(warnBuf.String(), "disk2") {
		t.Fatalf("非默认卷 AuditTTL 应有 Warn（禁静默忽略）, got: %s", warnBuf.String())
	}
}

// TestVolumeRetention_Pass_ExpiredCleanup 核心变异点：fixture 卷放过期/未过期版本 +
// 分享 token + 审计行，手动触发一次 GC → 过期删除、未过期保留。
func TestVolumeRetention_Pass_ExpiredCleanup(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir()}
	auditDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0], Retention: VolumeRetentionConfig{
			VersionTTL: time.Hour, ShareTTL: time.Hour, AuditTTL: 2 * time.Hour, GCInterval: 10 * time.Minute,
		}},
		{Name: "disk2", Root: dirs[1], Retention: VolumeRetentionConfig{
			VersionTTL: time.Hour, GCInterval: 10 * time.Minute,
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// 审计 store：3 行，其中 1 行超 2h 窗口。
	auditSt, err := NewAuditStore(filepath.Join(auditDir, "audit.log"), testLogger())
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	now := time.Now()
	_ = auditSt.Append(AuditEvent{Action: "old", TS: now.Add(-3 * time.Hour)})
	_ = auditSt.Append(AuditEvent{Action: "keep1", TS: now.Add(-time.Hour)})
	_ = auditSt.Append(AuditEvent{Action: "keep2", TS: now.Add(-30 * time.Minute)})

	h := retentionTestHandlers(t, cfg, "", auditSt)
	tntMain := h.tenantFor("alice")
	if tntMain == nil || tntMain.Root() == nil {
		t.Fatal("alice 默认卷租户不可用")
	}
	seedVersionEntry(t, tntMain, "f.txt", 5*time.Hour)    // 过期
	seedVersionEntry(t, tntMain, "f.txt", 30*time.Minute) // 未过期
	disk2Tnt := h.volumeTenant("disk2", "bob")
	if disk2Tnt == nil || disk2Tnt.Root() == nil {
		t.Fatal("bob disk2 卷租户不可用")
	}
	seedVersionEntry(t, disk2Tnt, "g.txt", 3*time.Hour) // 过期（disk2 卷）

	// 分享：默认卷 anonymous/meta/share 持久化；一条 CreatedAt 超 ShareTTL（兜底删）、
	// 一条未超（保留）。
	shareDir := filepath.Join(dirs[0], "anonymous", "meta", "share")
	h.shareStore.EnablePersist(shareDir)
	seedRetentionShare(t, h, "old.txt", 3*time.Hour, 2*time.Hour)      // CreatedAt 3h ago > 1h TTL → 删
	seedRetentionShare(t, h, "fresh.txt", 30*time.Minute, 2*time.Hour) // CreatedAt 30m ago < 1h TTL → 保留

	if got := countVersionEntries(t, tntMain, "f.txt"); got != 2 {
		t.Fatalf("前置 f.txt 版本数=%d want 2", got)
	}
	if got := countVersionEntries(t, disk2Tnt, "g.txt"); got != 1 {
		t.Fatalf("前置 g.txt 版本数=%d want 1", got)
	}
	if got := countSharePersistFiles(t, shareDir); got != 2 {
		t.Fatalf("前置分享持久化文件数=%d want 2", got)
	}

	// 手动触发两卷各一轮 GC。
	h.volumeRetentionPassFor("main")
	h.volumeRetentionPassFor("disk2")

	if got := countVersionEntries(t, tntMain, "f.txt"); got != 1 {
		t.Fatalf("GC 后 f.txt 版本数=%d want 1（过期已清）", got)
	}
	if got := countVersionEntries(t, disk2Tnt, "g.txt"); got != 0 {
		t.Fatalf("GC 后 g.txt 版本数=%d want 0（disk2 卷全部过期）", got)
	}
	if got := countSharePersistFiles(t, shareDir); got != 1 {
		t.Fatalf("GC 后分享持久化文件数=%d want 1（仅 fresh 保留）", got)
	}
	if got := len(h.shareStore.List("")); got != 1 {
		t.Fatalf("GC 后分享内存条数=%d want 1（仅 fresh 保留）", got)
	}
	if got := auditSt.Len(); got != 2 {
		t.Fatalf("审计按龄清理后 Len()=%d want 2（窗口内保留）", got)
	}
	// 落盘审计日志同步重写：行数 == 保留数。
	data, rerr := os.ReadFile(filepath.Join(auditDir, "audit.log"))
	if rerr != nil {
		t.Fatalf("读审计日志: %v", rerr)
	}
	if got := len(strings.Split(strings.TrimSpace(string(data)), "\n")); got != 2 {
		t.Fatalf("审计日志行数=%d want 2（落盘同步重写）", got)
	}
}

// TestVolumeRetention_Disabled_NoGoroutine 全零 retention：不启动周期 goroutine、
// pass 无副作用（零回归）。
func TestVolumeRetention_Disabled_NoGoroutine(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	h := retentionTestHandlers(t, cfg, "", nil)
	if h.hasVolumeRetentionLoop() {
		t.Fatal("全零 retention 不应启动周期 GC goroutine")
	}
	// pass 无副作用：seed 版本文件保留。
	tnt := h.tenantFor("alice")
	seedVersionEntry(t, tnt, "f.txt", 5*time.Hour)
	h.volumeRetentionPassFor("default")
	if got := countVersionEntries(t, tnt, "f.txt"); got != 1 {
		t.Fatalf("无 retention 时 pass 后版本数=%d want 1（零回归）", got)
	}
	// RegisterRoutes 装配 + Close 不阻塞（无 goroutine 泄漏）由下方 HTTPRegister 用例覆盖。
}

// TestVolumeRetention_Disabled_HTTPRegister 经 RegisterRoutes 装配（全零 retention）：
// 服务可正常启动/关闭（无 retention goroutine，Close 不阻塞）。
func TestVolumeRetention_Disabled_HTTPRegister(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	ts := httptest.NewServer(h.Handler())
	ts.Close()
	_ = h.Close()
}
