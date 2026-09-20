// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
)

// writeContextFixture 写入含两 context（a=current / b）的临时 config.yaml，返回路径。
func writeContextFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := &contextcfg.Config{
		CurrentContext: "a",
		Environments: []*contextcfg.Environment{
			{Name: "a", ServerURL: "https://a:18083", HubURL: "wss://a:18083/ws", NodeID: "home"},
			{Name: "b", ServerURL: "https://b:18083"},
		},
		Users: []*contextcfg.User{
			{Name: "u1", AccessKey: "ak-1", AccessKeySecret: "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435", AccessKeyID: "skey-1", Owner: "u1"},
			{Name: "u2", AccessKey: "ak-2"},
		},
		Contexts: []*contextcfg.Context{
			{Name: "a", Environment: "a", User: "u1", Volume: "vol1"},
			{Name: "b", Environment: "a", User: "u1"},
		},
	}
	if err := contextcfg.Save(cfg, cfgPath); err != nil {
		t.Fatalf("Save fixture: %v", err)
	}
	return cfgPath
}

// runContextCmd 执行 context/env/user 命令（注入 cfgPath，捕获输出）。
func runContextCmd(t *testing.T, cfgPath string, args ...string) (string, error) {
	t.Helper()
	cmd := newCmdContext(&cfgPath)
	cmd.SetArgs(args)
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	err := cmd.Execute()
	return out.String(), err
}

// TestContextCmd_ListAndUse：list 列出全部 context（含 * 标记），use 切换 current。
func TestContextCmd_ListAndUse(t *testing.T) {
	cfgPath := writeContextFixture(t)
	out, err := runContextCmd(t, cfgPath, "list")
	if err != nil {
		t.Fatalf("context list: %v", err)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("list 应含全部 context: %q", out)
	}
	if !strings.Contains(out, "* a") {
		t.Errorf("list 应标 * 当前 context a: %q", out)
	}
	_, err = runContextCmd(t, cfgPath, "use", "b")
	if err != nil {
		t.Fatalf("context use: %v", err)
	}
	got, _ := contextcfg.Load(cfgPath)
	if got.CurrentContext != "b" {
		t.Errorf("use 后 current 应为 b, got %q", got.CurrentContext)
	}
}

// TestContextCmd_UseEmpty_Fails：空 context 名拒绝（M-4 裁定）。
func TestContextCmd_UseEmpty_Fails(t *testing.T) {
	cfgPath := writeContextFixture(t)
	_, err := runContextCmd(t, cfgPath, "use", "")
	if err == nil {
		t.Fatal("context use 空名应报错")
	}
	if !strings.Contains(err.Error(), "不能为空") {
		t.Errorf("报错应含空名提示: %v", err)
	}
}

// TestContextCmd_Get：显示解析合并视图（env server + user ak + volume）。
func TestContextCmd_Get(t *testing.T) {
	cfgPath := writeContextFixture(t)
	out, err := runContextCmd(t, cfgPath, "get")
	if err != nil {
		t.Fatalf("context get: %v", err)
	}
	if !strings.Contains(out, "server_url: https://a:18083") {
		t.Errorf("get 应含 env server_url: %q", out)
	}
	if !strings.Contains(out, "access_key: ak-1") {
		t.Errorf("get 应含 user access_key: %q", out)
	}
	if !strings.Contains(out, "volume: vol1") {
		t.Errorf("get 应含 volume: %q", out)
	}
	// 指定名字。
	out2, err := runContextCmd(t, cfgPath, "get", "b")
	if err != nil {
		t.Fatalf("context get b: %v", err)
	}
	if !strings.Contains(out2, "name: b") {
		t.Errorf("get b 应显示 b: %q", out2)
	}
}

// TestContextCmd_Set_CreateAndUpdate：新建（缺 env/user 报错）+ 更新（flag 覆盖）。
func TestContextCmd_Set_CreateAndUpdate(t *testing.T) {
	cfgPath := writeContextFixture(t)
	// 新建缺 --env-name → 报错。
	_, err := runContextCmd(t, cfgPath, "set", "c", "--user-name", "u1")
	if err == nil {
		t.Fatal("新建 context 缺 --env-name 应报错")
	}
	// 新建完整。
	_, err = runContextCmd(t, cfgPath, "set", "c", "--env-name", "b", "--user-name", "u2")
	if err != nil {
		t.Fatalf("context set 新建: %v", err)
	}
	cfg, _ := contextcfg.Load(cfgPath)
	ctxC := cfg.FindContext("c")
	if ctxC == nil || ctxC.Environment != "b" || ctxC.User != "u2" {
		t.Errorf("新建 context c 错: %+v", ctxC)
	}
	// 更新：只改 --user。
	_, err = runContextCmd(t, cfgPath, "set", "c", "--user-name", "u1")
	if err != nil {
		t.Fatalf("context set 更新: %v", err)
	}
	cfg, _ = contextcfg.Load(cfgPath)
	if cfg.FindContext("c").User != "u1" {
		t.Errorf("更新后 user 应为 u1: %+v", cfg.FindContext("c"))
	}
	// 引用不存在的 env → 报错。
	_, err = runContextCmd(t, cfgPath, "set", "c", "--env-name", "nope")
	if err == nil {
		t.Fatal("引用不存在 env 应报错")
	}
}

// TestContextCmd_Set_FlagNamesAvoidRootShadow：context set 的字段 flag 不得与 root 全局 flag 同名
// （root --env/--user/--volume 是全局 context 覆盖语义；context set 的字段写入 flag 若同名会本地覆盖父
// 导致语义错位——改名 --env-name/--user-name/--volume-name 根除）。
func TestContextCmd_Set_FlagNamesAvoidRootShadow(t *testing.T) {
	t.Parallel()
	cfgPath := writeContextFixture(t)
	// 新名生效：--env-name/--user-name 新建 context。
	_, err := runContextCmd(t, cfgPath, "set", "c", "--env-name", "b", "--user-name", "u2")
	if err != nil {
		t.Fatalf("context set 新建（新 flag 名）: %v", err)
	}
	cfg, _ := contextcfg.Load(cfgPath)
	ctxC := cfg.FindContext("c")
	if ctxC == nil || ctxC.Environment != "b" || ctxC.User != "u2" {
		t.Errorf("新建 context c 错: %+v", ctxC)
	}
	// 新名更新：--volume-name 覆盖卷字段。
	_, err = runContextCmd(t, cfgPath, "set", "c", "--volume-name", "volX")
	if err != nil {
		t.Fatalf("context set 更新 volume（新 flag 名）: %v", err)
	}
	cfg, _ = contextcfg.Load(cfgPath)
	if cfg.FindContext("c").Volume != "volX" {
		t.Errorf("更新后 volume 应为 volX: %+v", cfg.FindContext("c"))
	}
	// 旧名应报错（unknown flag）——不再被 root 同名 flag 静默接受。
	_, err = runContextCmd(t, cfgPath, "set", "d", "--env", "b", "--user", "u1")
	if err == nil {
		t.Fatal("旧 flag 名 --env/--user 应报 unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("旧 flag 名应报 unknown flag: %v", err)
	}
}

// TestContextCmd_DeleteCurrent_Fails：删除当前 context 拒绝。
func TestContextCmd_DeleteCurrent_Fails(t *testing.T) {
	cfgPath := writeContextFixture(t)
	_, err := runContextCmd(t, cfgPath, "delete", "a")
	if err == nil {
		t.Fatal("删除当前 context 应失败")
	}
	if !strings.Contains(err.Error(), "不能删除当前") {
		t.Errorf("报错应说明不能删当前: %v", err)
	}
	// 删除非当前成功。
	_, err = runContextCmd(t, cfgPath, "delete", "b")
	if err != nil {
		t.Fatalf("delete b: %v", err)
	}
	cfg, _ := contextcfg.Load(cfgPath)
	if cfg.FindContext("b") != nil {
		t.Error("delete 后 b 应不存在")
	}
}

// TestContextCmd_Rename：重命名并同步 current-context 指针。
func TestContextCmd_Rename(t *testing.T) {
	cfgPath := writeContextFixture(t)
	_, err := runContextCmd(t, cfgPath, "rename", "a", "a2")
	if err != nil {
		t.Fatalf("context rename: %v", err)
	}
	cfg, _ := contextcfg.Load(cfgPath)
	if cfg.FindContext("a2") == nil {
		t.Error("rename 后 a2 应存在")
	}
	if cfg.CurrentContext != "a2" {
		t.Errorf("current-context 应同步为 a2, got %q", cfg.CurrentContext)
	}
}

// TestEnvCmd_ListAndUse：env list 列出环境，use 更新当前 context 的 environment。
func TestEnvCmd_ListAndUse(t *testing.T) {
	cfgPath := writeContextFixture(t)
	var out strings.Builder
	cmd := newEnvCommand(&cfgPath)
	cmd.SetArgs([]string{"list"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env list: %v", err)
	}
	if !strings.Contains(out.String(), "a") || !strings.Contains(out.String(), "b") {
		t.Errorf("env list 应含全部环境: %q", out.String())
	}
	// use b → 当前 context a 的 environment 变 b。
	var out2 strings.Builder
	cmd2 := newEnvCommand(&cfgPath)
	cmd2.SetArgs([]string{"use", "b"})
	cmd2.SetOut(&out2)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("env use: %v", err)
	}
	got, _ := contextcfg.Load(cfgPath)
	if got.Contexts[0].Environment != "b" {
		t.Errorf("env use 应更新当前 context 的 environment: %+v", got.Contexts[0])
	}
	// 不存在环境 → 报错。
	var out3 strings.Builder
	cmd3 := newEnvCommand(&cfgPath)
	cmd3.SetArgs([]string{"use", "nope"})
	cmd3.SetOut(&out3)
	if err := cmd3.Execute(); err == nil {
		t.Fatal("env use 不存在环境应报错")
	}
}

// TestUserCmd_ListAndUse：user list 列出用户，use 更新当前 context 的 user。
func TestUserCmd_ListAndUse(t *testing.T) {
	cfgPath := writeContextFixture(t)
	var out strings.Builder
	cmd := newUserCommand(&cfgPath)
	cmd.SetArgs([]string{"list"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("user list: %v", err)
	}
	if !strings.Contains(out.String(), "u1") || !strings.Contains(out.String(), "u2") {
		t.Errorf("user list 应含全部用户: %q", out.String())
	}
	// use u2 → 当前 context a 的 user 变 u2。
	var out2 strings.Builder
	cmd2 := newUserCommand(&cfgPath)
	cmd2.SetArgs([]string{"use", "u2"})
	cmd2.SetOut(&out2)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("user use: %v", err)
	}
	got, _ := contextcfg.Load(cfgPath)
	if got.Contexts[0].User != "u2" {
		t.Errorf("user use 应更新当前 context 的 user: %+v", got.Contexts[0])
	}
}
