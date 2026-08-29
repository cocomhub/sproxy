// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdMeta 创建 meta 命令：查询文件元信息（含版本历史摘要）。
func NewCmdMeta(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta <filename>",
		Short: "查询文件元信息（含版本历史摘要）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			info, err := svc.Stat(cmd.Context(), filename)
			if err != nil {
				return fmt.Errorf("获取文件信息失败: %w", err)
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintStat(info, filename)
			return nil
		},
	}
	// 文件版本管理子命令
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "文件版本管理",
	}
	versionCmd.AddCommand(NewCmdMetaVersionList(factory, ios, st))
	versionCmd.AddCommand(NewCmdMetaVersionRestore(factory, ios, st))
	versionCmd.AddCommand(NewCmdMetaVersionDelete(factory, ios, st))
	cmd.AddCommand(versionCmd)
	return cmd
}

func NewCmdMetaVersionList(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "list <filename>",
		Short: "列出文件版本历史",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			versions, err := svc.ListVersions(cmd.Context(), filename)
			if err != nil {
				return err
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintVersionList(filename, versions)
			return nil
		},
	}
}

func NewCmdMetaVersionRestore(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <filename> <version_id>",
		Short: "恢复文件到指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("版本 ID 必须是数字: %w", err)
			}
			if err := svc.RestoreVersion(cmd.Context(), filename, v); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已恢复文件 '%s' 到版本 %s\n", filename, args[1])
			return nil
		},
	}
}

func NewCmdMetaVersionDelete(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <filename> <version_id>",
		Short: "删除文件的指定版本",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			filename, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("版本 ID 必须是数字: %w", err)
			}
			if err := svc.DeleteVersion(cmd.Context(), filename, v); err != nil {
				return err
			}
			fmt.Fprintf(ios.Out, "已删除文件 '%s' 的版本 %s\n", filename, args[1])
			return nil
		},
	}
}
