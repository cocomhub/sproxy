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

// --- 旧版全局变量（等待迁移到工厂模式） ---

var versionCmd = &cobra.Command{
	Use:   "version [subcommand]",
	Short: "显示版本信息或管理文件版本",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("sclient version %s (build: %s)\n", Version, BuildAt)
		fmt.Println()
		if cfgProvider != nil {
			cfg, err := client.LoadFromProvider(cfgProvider)
			if err == nil {
				client.HandleConfigShow(cfg)
			}
		}
	},
}

var versionListCmd = &cobra.Command{
	Use:   "list <filename>",
	Short: "列出文件的版本历史",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}
		versions, err := cli.ListVersions(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		fm := buildFormatter(cmd)
		fm.PrintVersionList(args[0], versions)
		return nil
	},
}

var versionRestoreCmd = &cobra.Command{
	Use:   "restore <filename> <version_id>",
	Short: "恢复文件到指定版本",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}
		if err := cli.RestoreVersion(cmd.Context(), args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("已恢复文件 '%s' 到版本 %s\n", args[0], args[1])
		return nil
	},
}

var versionDeleteCmd = &cobra.Command{
	Use:   "delete <filename> <version_id>",
	Short: "删除文件的指定版本",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}
		if err := cli.DeleteVersion(cmd.Context(), args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("已删除文件 '%s' 的版本 %s\n", args[0], args[1])
		return nil
	},
}

func init() {
	versionCmd.AddCommand(versionListCmd)
	versionCmd.AddCommand(versionRestoreCmd)
	versionCmd.AddCommand(versionDeleteCmd)
}

// --- 新版工厂函数（供迁移后使用） ---

// NewCmdVersion 创建 version 命令。
func NewCmdVersion(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		RunE: func(cmd *cobra.Command, args []string) error {
			ios.WriteOutLine("sclient %s (built at %s)", Version, BuildAt)
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
				return err
			}
			versions, err := svc.ListVersions(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(versions) == 0 {
				fmt.Fprintf(ios.Out, "文件 '%s' 没有历史版本\n", args[0])
				return nil
			}
			fmt.Fprintf(ios.Out, "文件 '%s' 的版本历史:\n", args[0])
			for _, v := range versions {
				checksum := v.Checksum
				if len(checksum) > 16 {
					checksum = checksum[:16] + "..."
				}
				fmt.Fprintf(ios.Out, "  ID: %d  Size: %d  Created: %s  Checksum: %s\n",
					v.VersionID, v.Size, v.CreatedAt, checksum)
			}
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
