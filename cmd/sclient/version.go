// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdVersion 创建 version 命令。
func NewCmdVersion(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version [subcommand]",
		Short: "显示版本信息",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(ios.Out, "sclient version %s (build: %s)\n", Version, BuildAt)
			// 尝试显示配置信息（如果 cfgProvider 可用）
			fmt.Fprintln(ios.Out)
			cfg, err := client.LoadFromProvider(cfgProvider)
			if err == nil {
				client.HandleConfigShow(cfg)
			}
			return nil
		},
	}
	cmd.AddCommand(NewCmdVersionList(factory, ios))
	cmd.AddCommand(NewCmdVersionRestore(factory, ios))
	cmd.AddCommand(NewCmdVersionDelete(factory, ios))
	return cmd
}

// NewCmdVersionList 创建 version list 命令。
func NewCmdVersionList(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "list <filename>",
		Short: "列出文件版本历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return err
			}
			versions, err := svc.ListVersions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintVersionList(args[0], versions)
			return nil
		},
	}
}

// NewCmdVersionRestore 创建 version restore 命令。
func NewCmdVersionRestore(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <filename> <version_id>",
		Short: "恢复文件到指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return err
			}
			if err := svc.RestoreVersion(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已恢复文件 '%s' 到版本 %s\n", args[0], args[1])
			return nil
		},
	}
}

// NewCmdVersionDelete 创建 version delete 命令。
func NewCmdVersionDelete(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <filename> <version_id>",
		Short: "删除文件的指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return err
			}
			if err := svc.DeleteVersion(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已删除文件 '%s' 的版本 %s\n", args[0], args[1])
			return nil
		},
	}
}
