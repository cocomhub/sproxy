// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_startup_test.go 锁定 M1-14（设计 docs/designs/2026-10-19-audit-service.md §5.6 点 6）：
// 审计的**启动期行为可配** —— 默认降级（审计绝不阻断启动）、显式 fail_fast 中止启动、
// 显式 disabled 完全关闭（**不建目录、不落盘**）。

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditStartupEnv 构造一个「审计落盘必失败」的配置：把 __audit__ 位置占成**普通文件**
// （NewFileJournal 的 MkdirAll 必失败）——用于区分 fail_fast 两种行为。
func auditStartupEnv(t *testing.T, mutate func(*Config)) (*Config, *slog.Logger) {
	t.Helper()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	if mutate != nil {
		mutate(cfg)
	}
	if !cfg.Audit.Disabled {
		blocker := filepath.Join(root, "__audit__")
		if err := os.WriteFile(blocker, []byte("blocker"), 0o644); err != nil {
			t.Fatalf("写占位文件: %v", err)
		}
	}
	return cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestAuditStartup_DefaultDegrades 默认（fail_fast=false）：落盘失败只降级，不 panic、
// 不阻断启动（审计绝不阻断启动 —— 既有行为零回归）。
func TestAuditStartup_DefaultDegrades(t *testing.T) {
	t.Parallel()
	cfg, log := auditStartupEnv(t, nil)
	svc, journal := setupAuditServices(cfg, log)
	if svc == nil {
		t.Fatal("默认应降级为仅内存（svc 非 nil），而不是完全关闭")
	}
	if journal != nil {
		t.Fatalf("落盘失败时 journal 应为 nil（降级），got %v", journal)
	}
	_ = svc.Close()
}

// TestAuditStartup_FailFastPanics fail_fast=true：落盘失败 ⇒ 中止启动（panic）。
// 这是「合规要求审计必须可用」的显式选择，避免静默失效。
func TestAuditStartup_FailFastPanics(t *testing.T) {
	t.Parallel()
	cfg, log := auditStartupEnv(t, func(c *Config) { c.Audit.Startup.FailFast = true })
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("fail_fast=true 时落盘失败必须 panic（中止启动）")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "fail_fast") {
			t.Fatalf("panic 信息应说明是 fail_fast 所致，got %v", r)
		}
	}()
	_, _ = setupAuditServices(cfg, log)
}

// TestAuditStartup_DisabledTouchesNothing disabled=true：完全关闭且**无副作用**
// （不创建任何审计目录/文件）——「关闭」不能变成「悄悄建目录并写盘」。
func TestAuditStartup_DisabledTouchesNothing(t *testing.T) {
	t.Parallel()
	cfg, log := auditStartupEnv(t, func(c *Config) { c.Audit.Disabled = true })
	svc, journal := setupAuditServices(cfg, log)
	if svc != nil || journal != nil {
		t.Fatalf("disabled 应返回 (nil, nil), got svc=%v journal=%v", svc, journal)
	}
	if _, err := os.Stat(filepath.Join(cfg.StorageRoot, "__audit__")); !os.IsNotExist(err) {
		t.Fatalf("disabled 不得创建审计目录（无副作用），stat err=%v", err)
	}
}
