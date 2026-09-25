// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdBackup 创建 backup 子命令：把卷导出为 tar 备份到本地（roadmap 11.7-5）。
//
//	sclient backup <vol> <dest>
//
// 服务端 GET /api/volumes/export（tar 流式 + 尾部 manifest.json）已就绪（#602）——
// 本命令只做 CLI 封装：调 ExportVolume → 流式写本地 dest。
// 卷名为空 = 全卷视图（服务端导出 owner 全部可见卷）。
func NewCmdBackup(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup <vol> <dest>",
		Short: "导出卷为 tar 备份到本地",
		Long: `把卷导出为 tar 备份到本地文件（服务端 GET /api/volumes/export 流式导出）。

	<vol> 指定卷名（留空 = 导出当前凭据可见的全部卷）；
	<dest> 为本地目标 .tar 文件路径（父目录不存在时自动创建；原子落盘）。

	导出 tar 含全部文件内容 + 尾部 manifest.json（每条目相对路径 + SHA-256 + size +
	mtime + 台账交叉校验），可用于跨实例迁移 / 恢复（服务端 POST /api/volumes/import）。`,
		Example: `  sclient backup disk2 backup-disk2.tar
  sclient backup "" all-volumes.tar   # 导出全部可见卷
  sclient backup disk2 -o /backup/disk2.tar`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			vol := args[0]
			dest, _ := cmd.Flags().GetString("output")
			if dest == "" && len(args) > 1 {
				dest = args[1]
			}
			if strings.TrimSpace(dest) == "" {
				return fmt.Errorf("目标文件路径不能为空（backup <vol> <dest>）")
			}

			if err := svc.ExportVolume(cmd.Context(), vol, dest); err != nil {
				ios.WriteErrLine("导出失败: %v", err)
				return fmt.Errorf("导出失败: %w", err)
			}
			volTxt := vol
			if volTxt == "" {
				volTxt = "<全部卷>"
			}
			ios.WriteOutLine("备份完成: %s（卷 %s）", dest, volTxt)
			return nil
		},
	}
	cmd.Flags().StringP("output", "o", "", "输出文件路径（也可作为第二参数 <dest> 传入）")
	return cmd
}
