// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// sighup_scope_test.go 钉住 SIGHUP 热更新范围契约：软配置（log_level/log_format）生效、
// 硬配置（addr/storage_root/owner_quotas/rate_limit 等）仅警告不生效（需重启进程）。
//
// 串行原因：操作包级变量 cfgProvider/cfgPtr/cfgFile，与 root_extra_test.go 的
// handleSighup 用例互斥（同一包级状态）。
// sproxy:serial: 操作包级 cfgProvider/cfgPtr/cfgFile，与既有 SIGHUP 用例互斥。

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
)

// captureSlog 把 slog 默认 handler 替换为写 buf 的 handler，返回恢复函数。
// 每个测试自建独立 buffer，避免测试间共享默认 logger 的竞态。
func captureSlog(buf *bytes.Buffer) func() {
	old := slog.Default()
	logger := slog.New(slog.NewTextHandler(buf, nil))
	slog.SetDefault(logger)
	return func() { slog.SetDefault(old) }
}

func TestSighup_SoftConfigApplied(t *testing.T) {
	// sproxy:serial: 操作包级 cfgProvider/cfgPtr，需与其它 handleSighup 用例互斥
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })
	cfgFile = cfgPath
	t.Cleanup(func() { cfgPtr.Store(nil) })
	cfgPtr.Store(initialCfg)

	// 修改软配置：log_level=debug + log_format=json
	newCfg := *initialCfg
	newCfg.LogLevel = "debug"
	newCfg.LogFormat = "json"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	handleSighup(initialCfg)

	reloaded := cfgPtr.Load()
	if reloaded.LogLevel != "debug" {
		t.Errorf("软配置 log_level 应生效, got %q", reloaded.LogLevel)
	}
	if reloaded.LogFormat != "json" {
		t.Errorf("软配置 log_format 应生效, got %q", reloaded.LogFormat)
	}
}

func TestSighup_HardConfigWarnsOnly(t *testing.T) {
	// sproxy:serial: 操作包级 cfgProvider/cfgPtr，需与其它 handleSighup 用例互斥
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = ":18083"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })
	cfgFile = cfgPath
	t.Cleanup(func() { cfgPtr.Store(nil) })
	cfgPtr.Store(initialCfg)

	// 修改硬配置：addr + storage_root + rate_limit（都应警告不生效）
	newCfg := *initialCfg
	newCfg.Addr = ":19000"
	newCfg.StorageRoot = filepath.Join(tmpDir, "other-storage")
	newCfg.RateLimit = server.RateLimitConfig{Enabled: true, Requests: 10, Window: 1}
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	handleSighup(initialCfg)

	// 三处硬配置都必须有「不会生效」警告
	for _, want := range []string{"addr 修改在 SIGHUP 后不会生效", "storage_root 修改在 SIGHUP 后不会生效", "rate_limit 修改在 SIGHUP 后不会生效"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("缺少硬配置警告 %q；输出:\n%s", want, buf.String())
		}
	}
}

func TestSighup_InvalidConfigKeepsOld(t *testing.T) {
	// sproxy:serial: 操作包级 cfgProvider/cfgPtr，需与其它 handleSighup 用例互斥
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })
	cfgFile = cfgPath
	t.Cleanup(func() { cfgPtr.Store(nil) })
	cfgPtr.Store(initialCfg)

	// 写入非法 YAML（语法错误 → Refresh 失败）
	if err := os.WriteFile(cfgPath, []byte("addr: \":18083\"\nlog_level: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	restore := captureSlog(&buf)
	defer restore()

	handleSighup(initialCfg)

	// 非法配置 → 保持旧值 + 报错日志
	if got := cfgPtr.Load().LogLevel; got != initialCfg.LogLevel {
		t.Errorf("非法配置后 LogLevel 应保持旧值, got %q", got)
	}
	if !strings.Contains(buf.String(), "SIGHUP config reload failed") {
		t.Errorf("非法配置应输出重载失败日志；输出:\n%s", buf.String())
	}
}
