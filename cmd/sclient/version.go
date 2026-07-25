// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

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
