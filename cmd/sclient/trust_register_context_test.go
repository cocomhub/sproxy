// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
)

// ---- trust register/login 作用于 context（T5：设计 §4.2）----

// newTrustContextEnv 构造 context 模式测试环境：config.yaml（contextcfg 三件套）
// 指向 mock 服务端；mock 端点同 trustLoginEnv（register/nonce/login）。
// 返回 env 便于断言 register/login 请求体、config.yaml 回填与 user 切换。
func newTrustContextEnv(t *testing.T, registerAdmin bool) *trustLoginEnv {
	t.Helper()
	env := newTrustLoginEnv(t, registerAdmin)
	// 覆盖配置：config.yaml（context 模型）替代平铺 sclient.yaml。
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	ccfg := contextcfg.NewDefault()
	ccfg.CurrentContext = "sg"
	ccfg.Environments = append(ccfg.Environments, &contextcfg.Environment{
		Name: "sg", ServerURL: env.srv.URL,
	})
	ccfg.Users = append(ccfg.Users, &contextcfg.User{Name: "alice"})
	ccfg.Contexts = append(ccfg.Contexts, &contextcfg.Context{
		Name: "sg", Environment: "sg", User: "alice",
	})
	if err := contextcfg.Save(ccfg, cfgPath); err != nil {
		t.Fatalf("save context config: %v", err)
	}
	env.cfgPath = cfgPath
	// svc 保持无凭据（M14）；execute 用 contextcfg 驱动的 cfgPath。
	return env
}

// contextConfig 重新加载 config.yaml（断言回填/切换结果）。
func (e *trustLoginEnv) contextConfig(t *testing.T) *contextcfg.Config {
	t.Helper()
	ccfg, err := contextcfg.Load(e.cfgPath)
	if err != nil {
		t.Fatalf("reload context config: %v", err)
	}
	return ccfg
}

// TestTrustRegister_ContextEnv_CreatesUserAndSwitches：register 用当前 context 的
// env（server_url）注册 → 成功后 Users 增加新用户（名=owner，ak 回填）+ 当前
// context 的 user 自动切到新用户 + 输出提示用 trust login <owner>。
func TestTrustRegister_ContextEnv_CreatesUserAndSwitches(t *testing.T) {
	env := newTrustContextEnv(t, false)
	out, _, err := env.register(t, "bob")
	if err != nil {
		t.Fatalf("trust register (context) failed: %v", err)
	}
	o := out.String()
	if !strings.Contains(o, "ak-totp-0123456789abcdef") {
		t.Errorf("输出应含注册 AK: %s", o)
	}
	if !strings.Contains(o, "trust login bob") {
		t.Errorf("应提示用 trust login bob 完成绑定: %s", o)
	}
	if env.regCalls != 1 {
		t.Errorf("register 应恰好调用 1 次, got %d", env.regCalls)
	}
	// config.yaml：Users 增加 bob（ak 回填、secret/id 空）+ context user 切到 bob。
	ccfg := env.contextConfig(t)
	u := ccfg.FindUser("bob")
	if u == nil {
		t.Fatalf("Users 应增加 bob: %+v", ccfg.Users)
	}
	if u.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("bob access_key 应回填: %q", u.AccessKey)
	}
	if u.AccessKeySecret != "" || u.AccessKeyID != "" {
		t.Errorf("register 无 session SK，不应回填 secret/id: %+v", u)
	}
	cur := ccfg.FindContext(ccfg.CurrentContext)
	if cur == nil || cur.User != "bob" {
		t.Errorf("当前 context 的 user 应自动切到 bob: %+v", cur)
	}
}

// TestTrustRegister_ContextEnv_ExistingUser_UpdatesInPlace：register 时 owner 已
// 存在于 Users → 复用该 user 段（不重复追加），仅更新 ak 并切换 context user。
func TestTrustRegister_ContextEnv_ExistingUser_UpdatesInPlace(t *testing.T) {
	env := newTrustContextEnv(t, false)
	ccfg := env.contextConfig(t)
	ccfg.Users = append(ccfg.Users, &contextcfg.User{Name: "bob", AccessKey: "ak-old", Owner: "bob"})
	if err := contextcfg.Save(ccfg, env.cfgPath); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := env.register(t, "bob"); err != nil {
		t.Fatalf("trust register failed: %v", err)
	}
	reloaded := env.contextConfig(t)
	var bobCount int
	for _, u := range reloaded.Users {
		if u.Name == "bob" {
			bobCount++
		}
	}
	if bobCount != 1 {
		t.Errorf("bob 应唯一存在（复用不追加）: count=%d", bobCount)
	}
	u := reloaded.FindUser("bob")
	if u.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("bob access_key 应更新为新注册 AK: %q", u.AccessKey)
	}
}

// TestTrustLogin_ContextUser_UsesCurrentUser：context 的 user = alice（已含凭据）
// → login 用当前 user 的 ak 登录，回填到该 user 段（secret/id 更新）。
func TestTrustLogin_ContextUser_UsesCurrentUser(t *testing.T) {
	env := newTrustContextEnv(t, false)
	// alice 预置 ak（供 login 默认使用）。
	ccfg := env.contextConfig(t)
	ccfg.FindUser("alice").AccessKey = "ak-totp-0123456789abcdef"
	if err := contextcfg.Save(ccfg, env.cfgPath); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, _, err := env.execute(t, totpTestCode+"\n")
	if err != nil {
		t.Fatalf("trust login (context) failed: %v", err)
	}
	if env.regCalls != 0 {
		t.Errorf("login 不应触发注册, regCalls=%d", env.regCalls)
	}
	if len(env.gotBodies) != 1 || env.gotBodies[0].AK != "ak-totp-0123456789abcdef" {
		t.Errorf("login body = %+v, want 当前 context user 的 ak", env.gotBodies)
	}
	if !strings.Contains(out.String(), "登录成功") {
		t.Errorf("输出应含登录成功: %s", out.String())
	}
	// 回填到 alice 段。
	reloaded := env.contextConfig(t)
	u := reloaded.FindUser("alice")
	if u == nil {
		t.Fatalf("alice 应存在: %+v", reloaded.Users)
	}
	if u.AccessKey != "ak-totp-0123456789abcdef" || u.AccessKeyID != env.skeyID {
		t.Errorf("alice 凭据未回填: %+v", u)
	}
	if u.AccessKeySecret != hex.EncodeToString(env.sessionSK) {
		t.Errorf("alice secret 应回填 session SK hex: %q", u.AccessKeySecret)
	}
}

// TestTrustLogin_ContextUser_UsernamePositional：位置参数用户名登录（context
// 模式）→ owner 反查登录 + 回填到该 user 段 + 自动切 context user。
func TestTrustLogin_ContextUser_UsernamePositional(t *testing.T) {
	env := newTrustContextEnv(t, false)
	out, _, err := env.execute(t, totpTestCode+"\n", "carol")
	if err != nil {
		t.Fatalf("trust login <username> (context) failed: %v", err)
	}
	if len(env.gotBodies) != 1 || env.gotBodies[0].Owner != "carol" {
		t.Errorf("位置参数用户名应走 owner 登录: %+v", env.gotBodies)
	}
	if !strings.Contains(out.String(), "登录成功") {
		t.Errorf("输出应含登录成功: %s", out.String())
	}
	reloaded := env.contextConfig(t)
	u := reloaded.FindUser("carol")
	if u == nil {
		t.Fatalf("carol 应创建: %+v", reloaded.Users)
	}
	if u.AccessKey != "ak-totp-0123456789abcdef" || u.AccessKeyID != env.skeyID {
		t.Errorf("carol 凭据未回填: %+v", u)
	}
	if cur := reloaded.FindContext(reloaded.CurrentContext); cur == nil || cur.User != "carol" {
		t.Errorf("context user 应自动切到 carol: %+v", cur)
	}
}

// TestTrustRegister_ContextServerURL：register 的 server_url 来自 context 的 env
// （mock 服务端收到请求即证明），且 config.yaml 存在时**不**经过平铺 cfgSvc。
func TestTrustRegister_ContextServerURL(t *testing.T) {
	env := newTrustContextEnv(t, false)
	if _, _, err := env.register(t, "dave"); err != nil {
		t.Fatalf("trust register failed: %v", err)
	}
	// register 请求到达 mock 服务端（server_url = context env 的 srv.URL）。
	if env.regCalls != 1 {
		t.Errorf("register 应恰好调用 1 次, got %d", env.regCalls)
	}
}

// TestTrustRegister_NoContext_FallsBackToFlat：config.yaml 不存在（context 模式
// 无配置）→ 回落旧平铺路径（register 用 cfgSvc 的 server_url，回填平铺配置）。
func TestTrustRegister_NoContext_FallsBackToFlat(t *testing.T) {
	env := newTrustLoginEnv(t, false)
	// 既有平铺 cfgPath（sclient.yaml）已配置 srv.URL。
	out, _, err := env.register(t, "eve")
	if err != nil {
		t.Fatalf("trust register (flat) failed: %v", err)
	}
	if !strings.Contains(out.String(), "ak-totp-0123456789abcdef") {
		t.Errorf("输出应含注册 AK: %s", out.String())
	}
	cfg := env.reload(t)
	if cfg.AccessKey != "ak-totp-0123456789abcdef" {
		t.Errorf("平铺回填 access_key: %q", cfg.AccessKey)
	}
	if cfg.AccessKeySecret != "" || cfg.AccessKeyID != "" {
		t.Errorf("平铺 register 不应回填 secret/id: %+v", cfg)
	}
}
