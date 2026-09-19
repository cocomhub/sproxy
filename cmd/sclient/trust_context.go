// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/pkg/cli"
)

// ---- context 模型对 trust 命令的解析辅助 ----

// trustContextServerURL 解析当前 context 的 server_url（config.yaml 存在且
// current-context 可解析时返回 (url, true) 表示 context 模式）。
// config.yaml 不存在 / 无 current-context / 解析失败 → (空, false) 回落旧平铺。
func trustContextServerURL(cfgFile *string, ios cli.IOStreams) (string, bool) {
	if cfgFile == nil || *cfgFile == "" {
		return "", false
	}
	cfg, err := contextcfg.Load(*cfgFile)
	if err != nil {
		// 配置损坏：回落旧路径（不阻断，与 root.go 的 resolveAndMigrateContext 语义一致）。
		return "", false
	}
	if len(cfg.Contexts) == 0 || cfg.CurrentContext == "" {
		return "", false
	}
	cur := cfg.FindContext(cfg.CurrentContext)
	if cur == nil {
		return "", false
	}
	env := cfg.FindEnvironment(cur.Environment)
	if env == nil || env.ServerURL == "" {
		return "", false
	}
	return env.ServerURL, true
}

// trustContextCurrentUser 返回当前 context 的 user 名（config.yaml 可解析时）。
// 返回 (userName, ok)：ok=false 表示非 context 模式或 current 无 user。
func trustContextCurrentUser(cfgFile *string) (string, bool) {
	if cfgFile == nil || *cfgFile == "" {
		return "", false
	}
	cfg, err := contextcfg.Load(*cfgFile)
	if err != nil || len(cfg.Contexts) == 0 || cfg.CurrentContext == "" {
		return "", false
	}
	cur := cfg.FindContext(cfg.CurrentContext)
	if cur == nil || cur.User == "" {
		return "", false
	}
	return cur.User, true
}
