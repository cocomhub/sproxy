// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/spf13/cobra"
)

// setXDGConfigHome 在测试中替换 xdg.ConfigHome 为临时目录，并注册恢复。
// 注意：本测试不使用 t.Parallel()，避免与其他读取 xdg.ConfigHome 的测试并发冲突。
func setXDGConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := xdg.ConfigHome
	xdg.ConfigHome = dir
	t.Cleanup(func() { xdg.ConfigHome = old })
	return dir
}

// newRootCmdForTest 构造隔离 stderr 的根命令（迁移提示/日志写 stderr，不污染测试输出）。
func newRootCmdForTest(errOut *strings.Builder) *cobra.Command {
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(errOut)
	root.SetIn(&bytes.Buffer{})
	return root
}

// TestRootContext_FlagsRegistered：--context/--env/--user 三个全局 flag 注册。
func TestRootContext_FlagsRegistered(t *testing.T) {
	// sproxy:serial: 读 SCLIENT_* 环境变量与 flag 默认值（环境变量全局态）。
	t.Setenv("SCLIENT_ENV", "")
	t.Setenv("SCLIENT_CONTEXT", "")
	t.Setenv("SCLIENT_USER", "")
	root := NewRootCmd()
	for _, name := range []string{"context", "env", "user"} {
		if f := root.PersistentFlags().Lookup(name); f == nil {
			t.Fatalf("--%s flag 未注册", name)
		}
	}
}

// TestRootContext_EnvVarsInjectDefaults：SCLIENT_CONTEXT/SCLIENT_ENV/SCLIENT_USER
// 环境变量作为 --context/--env/--user flag 的默认值注入（flag 未显式指定时）。
func TestRootContext_EnvVarsInjectDefaults(t *testing.T) {
	// sproxy:serial: 修改 xdg.ConfigHome 全局；且依赖 config.yaml 不存在（空模型不报错）。
	t.Setenv("SCLIENT_CONTEXT", "ctx-from-env")
	t.Setenv("SCLIENT_ENV", "env-from-env")
	t.Setenv("SCLIENT_USER", "user-from-env")
	dir := setXDGConfigHome(t) // 隔离 XDG 配置目录（继承 factory_test 的同名 helper）
	_ = dir
	root := NewRootCmd()
	// 不执行（PersistentPreRunE 在 Execute 时跑）——直接验证 flag 默认值逻辑：
	// root.go 的 flag 注册应把环境变量作为 DefValue（在 NewRootCmd 内解析）。
	ctxF := root.PersistentFlags().Lookup("context")
	envF := root.PersistentFlags().Lookup("env")
	userF := root.PersistentFlags().Lookup("user")
	if ctxF == nil || envF == nil || userF == nil {
		t.Fatal("flag 未注册")
	}
	if !strings.Contains(ctxF.DefValue, "ctx-from-env") {
		t.Errorf("--context 默认值应来自 SCLIENT_CONTEXT，got %q", ctxF.DefValue)
	}
	if !strings.Contains(envF.DefValue, "env-from-env") {
		t.Errorf("--env 默认值应来自 SCLIENT_ENV，got %q", envF.DefValue)
	}
	if !strings.Contains(userF.DefValue, "user-from-env") {
		t.Errorf("--user 默认值应来自 SCLIENT_USER，got %q", userF.DefValue)
	}
}

// TestRootContext_FirstRunMigratesLegacy：config.yaml 不存在但检测到旧
// ~/.config/sproxy/sclient.yaml → 自动迁移生成 config.yaml（含环境/用户/上下文），
// 并打印迁移提示；旧文件保留。
func TestRootContext_FirstRunMigratesLegacy(t *testing.T) {
	// sproxy:serial: 修改 xdg.ConfigHome 全局（setXDGConfigHome）。
	dir := setXDGConfigHome(t)
	legacy := filepath.Join(dir, "sproxy", "sclient.yaml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("server_url: https://hub:18083\naccess_key: ak-1\naccess_key_secret: s1\naccess_key_id: skey-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 触发 PersistentPreRunE（Execute 一个轻命令）。
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfgPath := filepath.Join(dir, "sproxy", "config.yaml")
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Fatalf("迁移后 config.yaml 应生成: %v", statErr)
	}
	cfg, err := contextcfg.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load config.yaml: %v", err)
	}
	if len(cfg.Environments) != 1 || cfg.Environments[0].ServerURL != "https://hub:18083" {
		t.Errorf("迁移环境字段错: %+v", cfg.Environments)
	}
	if len(cfg.Users) != 1 || cfg.Users[0].AccessKey != "ak-1" {
		t.Errorf("迁移用户字段错: %+v", cfg.Users)
	}
	if cfg.CurrentContext != "default" {
		t.Errorf("迁移 current-context 应为 default: %q", cfg.CurrentContext)
	}
	if !strings.Contains(errOut.String(), "已导入") || !strings.Contains(errOut.String(), "context") {
		t.Errorf("应打印迁移提示，got: %q", errOut.String())
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Errorf("旧 sclient.yaml 应保留（回滚）: %v", statErr)
	}
}

// TestRootContext_NoConfig_NoLegacy_NoError：config.yaml 与旧 sclient.yaml 都不存在
// → 空模型解析不报错（首启零配置场景）。
func TestRootContext_NoConfig_NoLegacy_NoError(t *testing.T) {
	// sproxy:serial: 修改 xdg.ConfigHome 全局（setXDGConfigHome）。
	setXDGConfigHome(t)
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute 不应报错（无配置首启）: %v", err)
	}
}
