// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "查看服务器统计信息",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		stats, err := cli.GetStats(cmd.Context())
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取统计信息失败: %v\n", err)
			return fmt.Errorf("获取统计信息失败: %w", err)
		}

		fm := buildFormatter(cmd)
		fm.PrintStats(stats)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
}
