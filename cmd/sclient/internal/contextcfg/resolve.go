// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package contextcfg

import (
	"fmt"
)

// ResolveArgs 是 context 解析输入（CLI flag / 环境变量注入后的最终值）。
type ResolveArgs struct {
	// Context 显式指定完整上下文名（--context）；非空时整体选中，忽略
	// Environment/User 单项覆盖。
	Context string
	// Environment 显式指定环境名（--env / SCLIENT_ENV）；仅覆盖 current 的环境。
	Environment string
	// User 显式指定用户名（--user / SCLIENT_USER）；仅覆盖 current 的用户。
	User string
}

// Resolved 是解析后的有效上下文（env + user 都解析成功才有效）。
type Resolved struct {
	Environment *Environment
	User        *User
	Volume      string
	ContextName string
}

// Resolve 按优先级解析有效上下文：
//
//	--context > （--env + --user 单项覆盖）> current-context。
//
// 任一解析出的 environment/user 引用不存在 → 报错；两者都解析成功才返回
// Resolved。current-context 缺失且无任何 flag → 报错并指引 `sclient context use`。
func Resolve(cfg *Config, args ResolveArgs) (*Resolved, error) {
	if cfg == nil {
		return nil, fmt.Errorf("配置为空（请先 sclient context use <name> 或配置 config.yaml）")
	}
	if args.Context != "" {
		ctx := cfg.FindContext(args.Context)
		if ctx == nil {
			return nil, fmt.Errorf("context %q 不存在（sclient context list 查看）", args.Context)
		}
		return resolveNamed(cfg, ctx)
	}

	// 无 --context：先确定 base（current-context），再单项覆盖。
	baseName := cfg.CurrentContext
	if baseName == "" && args.Environment == "" && args.User == "" {
		return nil, fmt.Errorf("未指定 context/env/user 且 current-context 未设置：请先运行 sclient context use <name>")
	}
	var base *Context
	if baseName != "" {
		base = cfg.FindContext(baseName)
		if base == nil {
			return nil, fmt.Errorf("current-context %q 不存在（sclient context use <name> 重新设置）", baseName)
		}
	} else {
		// current 缺失但有单项覆盖：从一个「空 base」开始，仅当两项都给定才算完整。
		base = &Context{}
	}
	envName := base.Environment
	userName := base.User
	if args.Environment != "" {
		envName = args.Environment
	}
	if args.User != "" {
		userName = args.User
	}
	if envName == "" || userName == "" {
		return nil, fmt.Errorf("无法解析完整上下文：environment=%q user=%q（请用 --env/--user 指定，或 sclient context use <name>）", envName, userName)
	}
	res := &Resolved{
		Environment: cfg.FindEnvironment(envName),
		User:        cfg.FindUser(userName),
		Volume:      base.Volume,
		ContextName: baseName,
	}
	if res.Environment == nil {
		return nil, fmt.Errorf("environment %q 不存在（sclient env list 查看）", envName)
	}
	if res.User == nil {
		return nil, fmt.Errorf("user %q 不存在（sclient user list 查看）", userName)
	}
	return res, nil
}

// resolveNamed 解析指定 context（--context 路径）：env/user 引用缺失报错。
func resolveNamed(cfg *Config, ctx *Context) (*Resolved, error) {
	env := cfg.FindEnvironment(ctx.Environment)
	if env == nil {
		return nil, fmt.Errorf("context %q 引用的 environment %q 不存在", ctx.Name, ctx.Environment)
	}
	user := cfg.FindUser(ctx.User)
	if user == nil {
		return nil, fmt.Errorf("context %q 引用的 user %q 不存在", ctx.Name, ctx.User)
	}
	return &Resolved{
		Environment: env,
		User:        user,
		Volume:      ctx.Volume,
		ContextName: ctx.Name,
	}, nil
}
