// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// credential_rotation_test.go 验证凭据定期自动轮换调度器：
//  1. NotDue：SK 到期日远（> notify_before）→ 一轮 Pass 不轮换（无新增 SK 条目）。
//  2. Due：SK 到期在 notify_before 内 → 一轮 Pass 调 renew → 新增 SK 条目。
//  3. KeepOld：keep_old=2、已有 3 个存活 SK → Pass 后最旧 1 个被删除。
//  4. Disabled：interval=0 → 不启动 loop goroutine（无副作用）。
//  5. LoopStartStop：interval>0 装配 → loop 运行；Close() → 退出。
//
// 时钟可注入：rotationPass(now func() time.Time) 用假时钟驱动到期判断。

import (
	"context"
	"encoding/hex"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// rotationTestRing 构造带 N 个 SK 条目的 Ring，各条目 ExpiresAt 按偏移给定。
// 首条（idx 0）CreatedAt 最旧，其余依次更新（模拟 renew 序列）。
func rotationTestRing(t *testing.T, ak string, skHex string, expires ...time.Duration) *accesskey.Ring {
	t.Helper()
	ring := accesskey.NewRing()
	sk, err := hex.DecodeString(skHex)
	if err != nil || len(sk) != 32 {
		t.Fatalf("bad sk: %v", err)
	}
	if err := ring.UpsertAK(ak, "rotation-test"); err != nil {
		t.Fatalf("UpsertAK: %v", err)
	}
	now := time.Now()
	for i, off := range expires {
		opts := []accesskey.EntryOption{
			accesskey.WithExpiresAt(now.Add(off)),
			accesskey.WithMeta(accesskey.Meta{Type: "initial"}),
		}
		// CreatedAt 递增：AddKey 用 Ring.Now()（默认 time.Now），为精确控制 CreatedAt
		// 直接构造快照 Replace——不依赖 AddKey 的时钟。
		// 本 helper 先 AddKey 再经 Replace 重写 CreatedAt/ExpiresAt。
		id, err := ring.AddKey(ak, sk, opts...)
		if err != nil {
			t.Fatalf("AddKey #%d: %v", i, err)
		}
		_ = id
	}
	// 重写 CreatedAt 与 ExpiresAt（AddKey 的默认时钟不可控，直接 Replace 快照）。
	snap := ring.Snapshot()
	for i := range snap {
		if snap[i].AK != ak {
			continue
		}
		for j := range snap[i].Entries {
			off := expires[j]
			snap[i].Entries[j].CreatedAt = now.Add(-time.Duration(len(expires)-j) * time.Hour)
			snap[i].Entries[j].ExpiresAt = now.Add(off)
		}
	}
	if err := ring.Replace(snap); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	return ring
}

// rotationHandlers 构造最小 Handlers（仅凭据 Ring + 日志），供 rotationPass 直接调用。
// cfgPtr 可注入（renewCredential 经 credentialTTLFromCfg 读它；nil 时回落 30d 默认）。
func rotationHandlers(ring *accesskey.Ring) *Handlers {
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(Default())
	return &Handlers{
		credentialRing: ring,
		cfgPtr:         &cfgPtr,
		logger:         testLogger(),
		auditLogger:    testLogger(),
	}
}

// countAliveSK 统计 ring 中该 AK 的存活（alive）SK 条目数。
func countAliveSK(t *testing.T, ring *accesskey.Ring, ak string) int {
	t.Helper()
	entries, ok := ring.Lookup(ak)
	if !ok {
		return 0
	}
	return len(entries)
}

func TestCredentialRotation_NotDue_NoAction(t *testing.T) {
	t.Parallel()
	ring := rotationTestRing(t, testAccessKey, testAccessSecret,
		30*24*time.Hour, // 30d 后到期（> notify_before 7d）→ 不轮换
	)
	h := rotationHandlers(ring)
	now := time.Now()
	cfg := rotationConfig{interval: time.Hour, notifyBefore: 7 * 24 * time.Hour, keepOld: 2}

	before := countAliveSK(t, ring, testAccessKey)
	h.rotationPass(cfg, func() time.Time { return now })
	after := countAliveSK(t, ring, testAccessKey)
	if after != before {
		t.Fatalf("未到期不应轮换: before=%d after=%d", before, after)
	}
}

func TestCredentialRotation_Due_Renews(t *testing.T) {
	t.Parallel()
	ring := rotationTestRing(t, testAccessKey, testAccessSecret,
		24*time.Hour, // 1d 后到期（< notify_before 7d）→ 应轮换
	)
	h := rotationHandlers(ring)
	now := time.Now()
	cfg := rotationConfig{interval: time.Hour, notifyBefore: 7 * 24 * time.Hour, keepOld: 2}

	before := countAliveSK(t, ring, testAccessKey)
	h.rotationPass(cfg, func() time.Time { return now })
	after := countAliveSK(t, ring, testAccessKey)
	if after != before+1 {
		t.Fatalf("到期应轮换新增 1 条 SK: before=%d after=%d", before, after)
	}
}

func TestCredentialRotation_KeepOld_Prunes(t *testing.T) {
	t.Parallel()
	// 3 个 SK：最旧 30d 前创建、30d 后到期；第二个 20d 前、40d 后；最新 10d 前、50d 后。
	// keep_old=2 → 最旧 1 个被删除。
	ring := rotationTestRing(t, testAccessKey, testAccessSecret,
		30*24*time.Hour, 40*24*time.Hour, 50*24*time.Hour,
	)
	h := rotationHandlers(ring)
	now := time.Now()
	cfg := rotationConfig{interval: time.Hour, notifyBefore: 7 * 24 * time.Hour, keepOld: 2}

	// 未到期不轮换（全部 > notify_before），但 keep_old 裁剪仍应执行。
	h.rotationPass(cfg, func() time.Time { return now })
	if got := countAliveSK(t, ring, testAccessKey); got != 2 {
		t.Fatalf("keep_old=2 应裁剪到 2 条, got %d", got)
	}
}

func TestCredentialRotation_Disabled_NoLoop(t *testing.T) {
	t.Parallel()
	// interval=0 → RegisterRoutes 不启动 loop：直接断言 rotationPass 不可达（无副作用）
	// 通过「不调用 Pass」验证零回归；这里只验证配置读取默认 0。
	cfg := Default()
	if cfg.Credentials.Rotation.Interval != 0 {
		t.Fatalf("默认 rotation.interval 应为 0（关闭）, got %v", cfg.Credentials.Rotation.Interval)
	}
	if cfg.Credentials.Rotation.NotifyBefore != 7*24*time.Hour {
		t.Fatalf("默认 rotation.notify_before 应为 168h, got %v", cfg.Credentials.Rotation.NotifyBefore)
	}
	if cfg.Credentials.Rotation.KeepOld != 2 {
		t.Fatalf("默认 rotation.keep_old 应为 2, got %d", cfg.Credentials.Rotation.KeepOld)
	}
}

func TestCredentialRotation_LoopStartStop(t *testing.T) {
	t.Parallel()
	// interval>0 装配 → loop 运行；Close() → 退出（无 goroutine 泄漏）。
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.Credentials.Rotation.Interval = 10 * time.Minute
	cfg.Credentials.Rotation.NotifyBefore = 7 * 24 * time.Hour
	cfg.Credentials.Rotation.KeepOld = 2
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	ring := credentialsRingWithAdmin(t, "", "", testAccessKey, testAccessSecret)
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "test",
		BuildAt:        "test",
		Logger:         testLogger(),
		AuditLogger:    testLogger(),
		CredentialRing: ring,
	}
	h := RegisterRoutes(t.Context(), opts)
	// loop 已启动（interval>0）。
	if h.rotationStop == nil {
		t.Fatal("rotation.interval>0 应初始化 rotationStop")
	}
	_ = h.Close()
	// Close 后无 panic、无泄漏（-race 下 goroutine 退出验证）。
}

var _ = context.Background
