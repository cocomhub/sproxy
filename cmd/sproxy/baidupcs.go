// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
)

const (
	flagPCSBDUSS      = "bduss"
	flagPCSBinaryPath = "binary"
	flagPCSRoot       = "root"
	defaultPCSRoot    = "/"
)

// newCmdBaidupcs 创建 `sproxy baidupcs` 子命令：百度网盘存储后端自检/状态命令。
//
// 用法：
//
//	sproxy baidupcs --bduss <BDUSS> [--binary /path/to/BaiduPCS-Go] [--root /baidu]
//
// 构造 baidupcs Storage（二进制优先 + 库兜底），输出后端状态。
// 凭据走命令行传入（BDUSS），装配方后续可扩展为凭据 Ring。
func newCmdBaidupcs() *cobra.Command {
	var bduss, binaryPath, root string
	cmd := &cobra.Command{
		Use:   "baidupcs",
		Short: "百度网盘存储后端（BaiduPCS plugin）自检命令",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewTextHandler(cmd.OutOrStdout(), nil))
			storage, err := baidupcs.DefaultFactory().New(baidupcs.StorageConfig{
				Root:       root,
				BDUSS:      bduss,
				BinaryPath: binaryPath,
				Logger:     logger,
			})
			if err != nil {
				return fmt.Errorf("构造百度网盘后端失败: %w", err)
			}
			_ = storage
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"百度网盘后端就绪：root=%s binary=%s 二进制优先+库兜底\n",
				root, binaryPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&bduss, flagPCSBDUSS, "", "百度 BDUSS 凭据（必填）")
	cmd.Flags().StringVar(&binaryPath, flagPCSBinaryPath, "", "BaiduPCS-Go 可执行路径（空=PATH 查找）")
	cmd.Flags().StringVar(&root, flagPCSRoot, defaultPCSRoot, "网盘根路径")
	return cmd
}
