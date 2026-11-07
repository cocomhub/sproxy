// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
)

// ---- 统一 context 配置（单一事实源）----
//
// 背景：v1 存在「新旧双配置源」——`--config` 指向旧平铺 sclient.yaml，context 模型
// 另用独立路径 config.yaml（`configYAMLPath`）。后果：`trust login` 回填 config.yaml
// 而 `sclient config` 读 sclient.yaml ⇒ 用户看到「新凭据已写入但 config 仍显旧值」。
// 统一后：**唯一**配置文件 = config.yaml（`--config` 默认即指向它），无平铺回落。

// TestUnifiedConfig_DefaultsToConfigYAML：--config 默认值必须是 config.yaml
// （不再是 sclient.yaml），且不再有独立的 context 模型路径。
func TestUnifiedConfig_DefaultsToConfigYAML(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	setXDGConfigHome(t)
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.HasSuffix(flag.DefValue, "config.yaml") {
		t.Fatalf("--config 默认值应为 config.yaml，got %q", flag.DefValue)
	}
	if strings.Contains(flag.DefValue, "sclient.yaml") {
		t.Fatalf("--config 默认值不应再指向平铺 sclient.yaml，got %q", flag.DefValue)
	}
}

// TestUnifiedConfig_SCLIENT_ENV_DoesNotChangePath：SCLIENT_ENV 只作 --env 默认值，
// **不再**改变配置文件路径（根除「login 写 config.yaml、其他命令读 sclient.<env>.yaml」）。
func TestUnifiedConfig_SCLIENT_ENV_DoesNotChangePath(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "prod")
	setXDGConfigHome(t)
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.HasSuffix(flag.DefValue, "config.yaml") {
		t.Fatalf("SCLIENT_ENV=prod 时 --config 仍应为 config.yaml，got %q", flag.DefValue)
	}
	if strings.Contains(flag.DefValue, "prod") {
		t.Fatalf("SCLIENT_ENV 不应影响配置文件路径，got %q", flag.DefValue)
	}
	// 但 --env flag 默认值仍来自 SCLIENT_ENV（env 名覆盖语义保留）。
	if envF := root.PersistentFlags().Lookup("env"); envF == nil || envF.DefValue != "prod" {
		t.Fatalf("--env 默认值应仍来自 SCLIENT_ENV，got %v", envF)
	}
}

// TestUnifiedConfig_NoConfigYAML_ServerCommandErrors：无 config.yaml（也无旧文件）
// 时，连服务端的命令（list）必须在 PersistentPreRunE 阶段报错并指引创建 context。
func TestUnifiedConfig_NoConfigYAML_ServerCommandErrors(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	setXDGConfigHome(t)
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"list"})
	err := root.Execute()
	if err == nil {
		t.Fatal("无 config.yaml 时 list 应报错（fail-closed）")
	}
	msg := err.Error() + errOut.String()
	if !strings.Contains(msg, "config.yaml") {
		t.Errorf("错误应提示 config.yaml，got: %q", msg)
	}
	if !strings.Contains(msg, "context") {
		t.Errorf("错误应指引创建 context，got: %q", msg)
	}
}

// TestUnifiedConfig_NoConfigYAML_LocalCommandOK：无 config.yaml 时本地命令
// （version）不受门禁影响，可正常执行。
func TestUnifiedConfig_NoConfigYAML_LocalCommandOK(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	setXDGConfigHome(t)
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("无 config.yaml 时 version 应可运行: %v\nstderr:\n%s", err, errOut.String())
	}
}

// TestUnifiedConfig_WriteThenRead_SameFile：核心回归——写入（context 命令）与读取
// （config show 的 ConfigProvider）必须是**同一文件**。写入 user 凭据后，ConfigProvider
// （config show 的数据源）必须立刻看到新值（v1 的 bug 正是两者读不同文件）。
func TestUnifiedConfig_WriteThenRead_SameFile(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	dir := setXDGConfigHome(t)
	cfgPath := filepath.Join(dir, "sproxy", "config.yaml")
	if mkErr := os.MkdirAll(filepath.Dir(cfgPath), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	ccfg := contextcfg.NewDefault()
	ccfg.CurrentContext = "sg"
	ccfg.Environments = append(ccfg.Environments, &contextcfg.Environment{Name: "sg", ServerURL: "https://hub:18083"})
	ccfg.Users = append(ccfg.Users, &contextcfg.User{Name: "alice"})
	ccfg.Contexts = append(ccfg.Contexts, &contextcfg.Context{Name: "sg", Environment: "sg", User: "alice"})
	if err := contextcfg.Save(ccfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	// 运行 root（触发 PersistentPreRunE 解析 → resolvedContext 注入）。
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resolvedContext == nil {
		t.Fatal("resolvedContext 应非 nil（config.yaml 存在且可解析）")
	}

	// ConfigProvider（config show 数据源）读到的 server_url 必须来自同一 config.yaml。
	svc := &cliConfigProvider{}
	cfg, err := svc.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "https://hub:18083" {
		t.Fatalf("ConfigProvider 应读同一 config.yaml 的 env.server_url，got %q", cfg.ServerURL)
	}
}

// TestUnifiedConfig_SealfileSectionInConfigYAML：sealfile 配置段迁入 config.yaml
// 顶层（与 environments/users/contexts 平级），ConfigProvider 从这里读回落值。
func TestUnifiedConfig_SealfileSectionInConfigYAML(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	dir := setXDGConfigHome(t)
	cfgPath := filepath.Join(dir, "sproxy", "config.yaml")
	if mkErr := os.MkdirAll(filepath.Dir(cfgPath), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	body := `apiVersion: sclient/v1
kind: Config
current-context: sg
sealfile:
  key: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  out_dir: "/tmp/sealed"
  owner: bob
environments:
  - name: sg
    server_url: https://hub:18083
users:
  - name: alice
contexts:
  - name: sg
    environment: sg
    user: alice
`
	if werr := os.WriteFile(cfgPath, []byte(body), 0o600); werr != nil {
		t.Fatal(werr)
	}

	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	svc := &cliConfigProvider{cfgFile: cfgPath}
	var sfc sealfileCfg
	if err := svc.LoadSealfileConfig(&sfc); err != nil {
		t.Fatalf("LoadSealfileConfig: %v", err)
	}
	if sfc.Key != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Errorf("sealfile.key 应从 config.yaml 顶层读出，got %q", sfc.Key)
	}
	if sfc.OutDir != "/tmp/sealed" {
		t.Errorf("sealfile.out_dir 应从 config.yaml 顶层读出，got %q", sfc.OutDir)
	}
	if sfc.Owner != "bob" {
		t.Errorf("sealfile.owner 应从 config.yaml 顶层读出，got %q", sfc.Owner)
	}
}

// TestUnifiedConfig_LegacyMigrationStillWorks：一次性迁移保留——无 config.yaml
// 但存在旧平铺 sclient.yaml 时自动导入为 config.yaml（之后旧文件不再被读）。
func TestUnifiedConfig_LegacyMigrationStillWorks(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	dir := setXDGConfigHome(t)
	legacy := filepath.Join(dir, "sproxy", "sclient.yaml")
	if mkErr := os.MkdirAll(filepath.Dir(legacy), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	if werr := os.WriteFile(legacy, []byte("server_url: https://legacy:18083\naccess_key: ak-legacy\naccess_key_secret: s1\naccess_key_id: skey-1\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	var errOut strings.Builder
	root := newRootCmdForTest(&errOut)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cfgPath := filepath.Join(dir, "sproxy", "config.yaml")
	cfg, lerr := contextcfg.Load(cfgPath)
	if lerr != nil {
		t.Fatalf("迁移后 config.yaml 应可加载: %v", lerr)
	}
	if len(cfg.Environments) != 1 || cfg.Environments[0].ServerURL != "https://legacy:18083" {
		t.Fatalf("迁移应携带 server_url: %+v", cfg.Environments)
	}
	if !strings.Contains(errOut.String(), "已导入") {
		t.Errorf("应打印迁移提示，got %q", errOut.String())
	}
}
