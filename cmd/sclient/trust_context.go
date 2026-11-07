// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
)

// ---- context 模型对 trust 命令的解析辅助 ----

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
