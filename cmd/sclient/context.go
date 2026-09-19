// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/spf13/cobra"
)

// ---- sclient context / env / user 命令族（多环境多用户上下文切换）----
//
// 对标 kubectl/kubecm：contexts[]（environment+user 组合）+ current-context 指针
// 全部读写 ~/.config/sproxy/config.yaml（contextcfg.Load/Save/SetCurrentContext）。
//
// 子命令：
//   context list                列出全部 context（标 * 当前）
//   context use <name>          切换 current-context（空名拒绝）
//   context get [name]          显示解析后的合并视图（env+user+volume）
//   context set <name> --env <e> --user <u> [--volume <v>]  创建/更新
//   context delete <name>       删除 context（current 拒绝）
//   context rename <old> <new>  重命名
//   env list / env use <name>   环境列表 / 更新当前 context 的 environment
//   user list / user use <name> 用户列表 / 更新当前 context 的 user

// newCmdContext 创建 context 命令族（读写 config.yaml，无需 network/凭据）。
func newCmdContext(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "上下文管理（多环境多用户切换；对标 kubectl context）",
		Long:  "查看或切换 sclient 上下文（environments/users/contexts 三件套 + current-context）。\n子命令: list / use / get / set / delete / rename",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return nil
		},
	}
	cmd.AddCommand(newCmdContextList(cfgPath))
	cmd.AddCommand(newCmdContextUse(cfgPath))
	cmd.AddCommand(newCmdContextGet(cfgPath))
	cmd.AddCommand(newCmdContextSet(cfgPath))
	cmd.AddCommand(newCmdContextDelete(cfgPath))
	cmd.AddCommand(newCmdContextRename(cfgPath))
	return cmd
}

// newEnvCommand 创建 env 命令族（环境列表 / 切换当前 context 的环境）。
func newEnvCommand(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "环境管理（environments 列表 / 切换当前 context 的环境）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return nil
		},
	}
	cmd.AddCommand(newEnvList(cfgPath))
	cmd.AddCommand(newEnvUse(cfgPath))
	return cmd
}

// newUserCommand 创建 user 命令族（用户列表 / 切换当前 context 的用户）。
func newUserCommand(cfgPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "用户管理（users 列表 / 切换当前 context 的用户）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return nil
		},
	}
	cmd.AddCommand(newUserList(cfgPath))
	cmd.AddCommand(newUserUse(cfgPath))
	return cmd
}

// ---- context list / use / get / set / delete / rename ----

// newCmdContextList 列出全部 context，当前项标 *。
func newCmdContextList(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部 context（标 * 当前）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(cfg.Contexts))
			for _, c := range cfg.Contexts {
				names = append(names, c.Name)
			}
			sort.Strings(names)
			for _, n := range names {
				mark := " "
				if n == cfg.CurrentContext {
					mark = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", mark, n)
			}
			return nil
		},
	}
}

// newCmdContextUse 切换 current-context（写 config.yaml；空名拒绝）。
func newCmdContextUse(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "切换 current-context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("context 名不能为空（sclient context list 查看可选名）")
			}
			if err := contextcfg.SetCurrentContext(*cfgPath, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "切换到 context %q\n", name)
			return nil
		},
	}
}

// newCmdContextGet 显示 context 的解析合并视图（env+user+volume 扁平展示）。
func newCmdContextGet(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get [name]",
		Short: "显示 context 解析后的合并视图（缺省=当前）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			name := cfg.CurrentContext
			if len(args) == 1 {
				name = args[0]
			}
			if name == "" {
				return fmt.Errorf("未指定 context 名且未设置 current-context（sclient context list 查看）")
			}
			ctx := cfg.FindContext(name)
			if ctx == nil {
				return fmt.Errorf("context %q 不存在（sclient context list 查看）", name)
			}
			env := cfg.FindEnvironment(ctx.Environment)
			user := cfg.FindUser(ctx.User)
			fmt.Fprintf(cmd.OutOrStdout(), "name: %s\n", ctx.Name)
			fmt.Fprintf(cmd.OutOrStdout(), "environment: %s\n", ctx.Environment)
			if env != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "server_url: %s\n", env.ServerURL)
				fmt.Fprintf(cmd.OutOrStdout(), "hub_url: %s\n", env.HubURL)
				fmt.Fprintf(cmd.OutOrStdout(), "node_id: %s\n", env.NodeID)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "user: %s\n", ctx.User)
			if user != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "access_key: %s\n", user.AccessKey)
				if user.AccessKeySecret != "" {
					fmt.Fprintln(cmd.OutOrStdout(), "access_key_secret: <已配置>")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "access_key_id: %s\n", user.AccessKeyID)
			}
			if ctx.Volume != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "volume: %s\n", ctx.Volume)
			}
			if env == nil || user == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "警告: 引用的 environment/user 不存在")
			}
			return nil
		},
	}
}

// newCmdContextSet 创建/更新 context（--env/--user/--volume 覆盖字段）。
func newCmdContextSet(cfgPath *string) *cobra.Command {
	var envName, userName, volume string
	cmd := &cobra.Command{
		Use:   "set <name> --env <e> --user <u> [--volume <v>]",
		Short: "创建或更新 context（env+user 组合）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("context 名不能为空")
			}
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			existing := cfg.FindContext(name)
			create := existing == nil
			if create {
				if envName == "" || userName == "" {
					return fmt.Errorf("新建 context %q 必须指定 --env 与 --user", name)
				}
				if cfg.FindEnvironment(envName) == nil {
					return fmt.Errorf("environment %q 不存在（sclient env list 查看）", envName)
				}
				if cfg.FindUser(userName) == nil {
					return fmt.Errorf("user %q 不存在（sclient user list 查看）", userName)
				}
				cfg.Contexts = append(cfg.Contexts, &contextcfg.Context{
					Name: name, Environment: envName, User: userName, Volume: volume,
				})
			} else {
				// 更新：flag 未指定则保持原值。
				if envName != "" {
					if cfg.FindEnvironment(envName) == nil {
						return fmt.Errorf("environment %q 不存在（sclient env list 查看）", envName)
					}
					existing.Environment = envName
				}
				if userName != "" {
					if cfg.FindUser(userName) == nil {
						return fmt.Errorf("user %q 不存在（sclient user list 查看）", userName)
					}
					existing.User = userName
				}
				// --volume 显式传（含空串）都允许；cobra flag Changed 判断。
				if f := cmd.Flags().Lookup("volume"); f != nil && f.Changed {
					existing.Volume = volume
				}
			}
			if err := contextcfg.Save(cfg, *cfgPath); err != nil {
				return err
			}
			if create {
				fmt.Fprintf(cmd.OutOrStdout(), "已创建 context %q: env=%s user=%s\n", name, envName, userName)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "已更新 context %q\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&envName, "env", "", "environment 名（新建必填）")
	cmd.Flags().StringVar(&userName, "user", "", "user 名（新建必填）")
	cmd.Flags().StringVar(&volume, "volume", "", "卷覆盖（可选）")
	return cmd
}

// newCmdContextDelete 删除 context（current-context 拒绝删除）。
func newCmdContextDelete(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "删除 context（current-context 不可删除，先 use 其它）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			name := args[0]
			if name == cfg.CurrentContext {
				return fmt.Errorf("不能删除当前 context %q（先 sclient context use <其它> 再删）", name)
			}
			if cfg.FindContext(name) == nil {
				return fmt.Errorf("context %q 不存在（sclient context list 查看）", name)
			}
			kept := cfg.Contexts[:0]
			for _, c := range cfg.Contexts {
				if c.Name != name {
					kept = append(kept, c)
				}
			}
			cfg.Contexts = kept
			if err := contextcfg.Save(cfg, *cfgPath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除 context %q\n", name)
			return nil
		},
	}
}

// newCmdContextRename 重命名 context（current-context 同步更新指针）。
func newCmdContextRename(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "重命名 context（current-context 指针同步更新）",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := args[0], strings.TrimSpace(args[1])
			if newName == "" {
				return fmt.Errorf("新 context 名不能为空")
			}
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			ctx := cfg.FindContext(oldName)
			if ctx == nil {
				return fmt.Errorf("context %q 不存在（sclient context list 查看）", oldName)
			}
			if cfg.FindContext(newName) != nil {
				return fmt.Errorf("context %q 已存在", newName)
			}
			ctx.Name = newName
			if cfg.CurrentContext == oldName {
				cfg.CurrentContext = newName
			}
			if err := contextcfg.Save(cfg, *cfgPath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已重命名 context %q → %q\n", oldName, newName)
			return nil
		},
	}
}

// ---- env list / use ----

// newEnvList 列出全部 environment。
func newEnvList(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部 environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			for _, e := range cfg.Environments {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", e.Name, e.ServerURL)
			}
			return nil
		},
	}
}

// newEnvUse 更新当前 context 的 environment（写 config.yaml）。
func newEnvUse(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "切换当前 context 的 environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			if cfg.FindEnvironment(envName) == nil {
				return fmt.Errorf("environment %q 不存在（sclient env list 查看）", envName)
			}
			if cfg.CurrentContext == "" {
				return fmt.Errorf("未设置 current-context（先 sclient context use <name>）")
			}
			cur := cfg.FindContext(cfg.CurrentContext)
			if cur == nil {
				return fmt.Errorf("current-context %q 不存在", cfg.CurrentContext)
			}
			cur.Environment = envName
			if err := contextcfg.Save(cfg, *cfgPath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "当前 context %q 的 environment 已切换为 %q\n", cur.Name, envName)
			return nil
		},
	}
}

// ---- user list / use ----

// newUserList 列出全部 user（凭据脱敏展示）。
func newUserList(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部 user（凭据脱敏）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			for _, u := range cfg.Users {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tak=%s\n", u.Name, u.AccessKey)
			}
			return nil
		},
	}
}

// newUserUse 更新当前 context 的 user（写 config.yaml）。
func newUserUse(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "切换当前 context 的 user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userName := args[0]
			cfg, err := contextcfg.Load(*cfgPath)
			if err != nil {
				return err
			}
			if cfg.FindUser(userName) == nil {
				return fmt.Errorf("user %q 不存在（sclient user list 查看）", userName)
			}
			if cfg.CurrentContext == "" {
				return fmt.Errorf("未设置 current-context（先 sclient context use <name>）")
			}
			cur := cfg.FindContext(cfg.CurrentContext)
			if cur == nil {
				return fmt.Errorf("current-context %q 不存在", cfg.CurrentContext)
			}
			cur.User = userName
			if err := contextcfg.Save(cfg, *cfgPath); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "当前 context %q 的 user 已切换为 %q\n", cur.Name, userName)
			return nil
		},
	}
}
