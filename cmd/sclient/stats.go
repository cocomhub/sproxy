// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/client"
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

		fmt.Printf("服务器统计（自启动以来）\n")
		fmt.Printf("磁盘使用:\n")
		fmt.Printf("  目录:     %s\n", stats.DiskUsage.UploadsDir)
		fmt.Printf("  文件数:   %d\n", stats.DiskUsage.TotalFiles)
		fmt.Printf("  总大小:   %s\n", client.FormatByte(float64(stats.DiskUsage.TotalSize)))

		if stats.DiskTotal > 0 {
			usedPct := float64(stats.DiskUsed) / float64(stats.DiskTotal) * 100
			fmt.Printf("  磁盘分区: %s / %s (%.1f%%)\n",
				client.FormatByte(float64(stats.DiskUsed)),
				client.FormatByte(float64(stats.DiskTotal)),
				usedPct)
		}

		fmt.Printf("\n请求统计:\n")
		fmt.Printf("  总请求数: %d\n", stats.RequestCounts.Total)
		fmt.Printf("  2xx:      %d\n", stats.RequestCounts.Xx2)
		fmt.Printf("  4xx:      %d\n", stats.RequestCounts.Xx4)
		fmt.Printf("  5xx:      %d\n", stats.RequestCounts.Xx5)
		fmt.Printf("  活跃连接: %d\n", stats.ActiveConns)

		fmt.Printf("\n传输统计:\n")
		fmt.Printf("  上传文件:   %d\n", stats.FilesUploaded)
		fmt.Printf("  上传字节:   %s\n", client.FormatByte(float64(stats.BytesUploaded)))
		fmt.Printf("  下载文件:   %d\n", stats.FilesDownloaded)
		fmt.Printf("  下载字节:   %s\n", client.FormatByte(float64(stats.BytesDownloaded)))
		fmt.Printf("  删除文件:   %d\n", stats.FilesDeleted)

		if stats.MaxStorageBytes > 0 {
			usagePct := float64(stats.StorageUsage) / float64(stats.MaxStorageBytes) * 100
			fmt.Printf("\n存储限制: %s / %s (%.1f%%)\n",
				client.FormatByte(float64(stats.StorageUsage)),
				client.FormatByte(float64(stats.MaxStorageBytes)),
				usagePct)
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
}
