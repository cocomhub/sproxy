// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

func newCmdVolumeCopy(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "copy <file> [--to-volume <v>]",
		Short: "跨卷复制文件（保留源）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			fromVol, _ := cmd.Flags().GetString("from-volume")
			if fromVol == "" {
				fromVol = svc.Volume()
			}
			if fromVol == "" {
				return fmt.Errorf("--from-volume 必填（当前无卷上下文）")
			}
			toVol, _ := cmd.Flags().GetString("to-volume")
			if toVol == "" {
				return fmt.Errorf("--to-volume 必填（复制目标卷）")
			}
			filename := args[0]
			if !isRemoteAbs(filename) {
				resolved, rerr := st.ResolveRemotePath(filename)
				if rerr != nil {
					return fmt.Errorf("解析路径失败: %w", rerr)
				}
				filename = resolved
			}
			if err := svc.CopyVolume(cmd.Context(), fromVol, toVol, filename); err != nil {
				ios.WriteErrLine("跨卷复制失败: %v", err)
				return fmt.Errorf("跨卷复制失败: %w", err)
			}
			ios.WriteOutLine("复制完成: %s（%s → %s）", filename, fromVol, toVol)
			return nil
		},
	}
	cmd.Flags().String("from-volume", "", "源卷名（缺省 = 当前卷上下文）")
	cmd.Flags().String("to-volume", "", "目标卷名（必填）")
	return cmd
}

func newCmdVolumeMove(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move <file> [--to-volume <v>]",
		Short: "跨卷移动文件（源被移除）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			fromVol, _ := cmd.Flags().GetString("from-volume")
			if fromVol == "" {
				fromVol = svc.Volume()
			}
			if fromVol == "" {
				return fmt.Errorf("--from-volume 必填（当前无卷上下文）")
			}
			toVol, _ := cmd.Flags().GetString("to-volume")
			if toVol == "" {
				return fmt.Errorf("--to-volume 必填（移动目标卷）")
			}
			filename := args[0]
			if !isRemoteAbs(filename) {
				resolved, rerr := st.ResolveRemotePath(filename)
				if rerr != nil {
					return fmt.Errorf("解析路径失败: %w", rerr)
				}
				filename = resolved
			}
			if err := svc.MoveVolume(cmd.Context(), fromVol, toVol, filename); err != nil {
				ios.WriteErrLine("跨卷移动失败: %v", err)
				return fmt.Errorf("跨卷移动失败: %w", err)
			}
			ios.WriteOutLine("移动完成: %s（%s → %s）", filename, fromVol, toVol)
			return nil
		},
	}
	cmd.Flags().String("from-volume", "", "源卷名（缺省 = 当前卷上下文）")
	cmd.Flags().String("to-volume", "", "目标卷名（必填）")
	return cmd
}

func newCmdVolumeRebalance(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rebalance",
		Short: "触发卷再平衡（from 卷文件迁往 to 卷）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			fromVol, _ := cmd.Flags().GetString("from-volume")
			toVol, _ := cmd.Flags().GetString("to-volume")
			if fromVol == "" || toVol == "" {
				return fmt.Errorf("--from-volume 与 --to-volume 均必填")
			}
			maxBytes, _ := cmd.Flags().GetInt64("max-bytes")
			res, err := svc.RebalanceVolume(cmd.Context(), fromVol, toVol, maxBytes)
			if err != nil {
				ios.WriteErrLine("卷再平衡失败: %v", err)
				return fmt.Errorf("卷再平衡失败: %w", err)
			}
			ios.WriteOutLine("卷再平衡已提交: %s（moved=%d bytes=%d remaining=%d）",
				res.Message, res.Moved, res.BytesMoved, res.Remaining)
			return nil
		},
	}
	cmd.Flags().String("from-volume", "", "源卷名（必填）")
	cmd.Flags().String("to-volume", "", "目标卷名（必填）")
	cmd.Flags().Int64("max-bytes", 0, "迁移字节上限（0/缺省 = 不限）")
	return cmd
}

// ensureVolumeOpsClient 编译期引用（防误删）。
