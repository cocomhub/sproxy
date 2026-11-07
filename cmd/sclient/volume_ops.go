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
			return runVolumeXfer(cmd, args, st, factory, ios, false)
		},
	}
	cmd.Flags().String(flagFromVolume, "", "源卷名（缺省 = 当前卷上下文）")
	cmd.Flags().String(flagToVolume, "", "目标卷名（必填）") // NOSONAR: S1192 — 旗标帮助文案
	return cmd
}

func newCmdVolumeMove(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move <file> [--to-volume <v>]",
		Short: "跨卷移动文件（源删除）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumeXfer(cmd, args, st, factory, ios, true)
		},
	}
	cmd.Flags().String(flagFromVolume, "", "源卷名（缺省 = 当前卷上下文）")
	cmd.Flags().String(flagToVolume, "", "目标卷名（必填）")
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
			fromVol, _ := cmd.Flags().GetString(flagFromVolume)
			toVol, _ := cmd.Flags().GetString(flagToVolume)
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
	cmd.Flags().String(flagFromVolume, "", "源卷名（必填）")
	cmd.Flags().String(flagToVolume, "", "目标卷名（必填）")
	cmd.Flags().Int64("max-bytes", 0, "迁移字节上限（0/缺省 = 不限）")
	return cmd
}

func newCmdVolumeRefresh(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh [卷名]",
		Short: "强制刷新卷缓存/索引（感知进程外改动；无参 = 全部卷）",
		Long: `强制刷新服务端的卷缓存/索引（S5，2026-10-27）。

		背景：卷状态缓存**默认永不过期**（2026-10-29 用户裁定）——本进程的写由事件写穿（下一次读
		立即可见），但**进程外**改动（运维直写盘、同根多实例、用网盘 App 直改、备份恢复）不会被
		自动感知。本命令是唯一的显式收敛入口：

		  sclient volume refresh          # 清空**全部卷**的状态缓存
		  sclient volume refresh vault    # 只清 vault（secretdata 等外部卷还会重扫内存索引）
		  sclient volume refresh vault:/  # 卷 URI 寻址（同一命令约定）

		本地卷同样适用（自 2026-10-28 起本地卷也走状态缓存）；幂等、随时可重复执行。`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vol := ""
			if len(args) == 1 {
				name, _, ok := splitVolumeURI(args[0])
				if !ok {
					return fmt.Errorf("卷名或卷 URI 无效: %q", args[0])
				}
				vol = name
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return fmt.Errorf(errFmtInitClient, err)
			}
			if rerr := svc.RefreshVolume(cmd.Context(), vol); rerr != nil {
				ios.WriteErrLine("卷刷新失败: %v", rerr)
				return fmt.Errorf("卷刷新失败: %w", rerr)
			}
			if vol == "" {
				ios.WriteOutLine("全部卷状态缓存已清空（下次读取将回源）")
				return nil
			}
			ios.WriteOutLine("卷缓存已刷新: %s", vol)
			return nil
		},
	}
	return cmd
}

// ensureVolumeOpsClient 编译期引用（防误删）。

// runVolumeXfer 是 `volume copy` / `volume move` 的共享实现（NewCmdX 的 RunE 抽出，降 CC）：
// 卷上下文归一（URI / 工作目录所在卷）→ 源卷解析（--from-volume 优先，回落卷上下文）→
// 目标卷必填 → 相对路径解析（`/` 开头 = 从根起算）→ 调域 API。
// isMove=true 为移动（源删除），false 为复制（保留源）。
func runVolumeXfer(cmd *cobra.Command, args []string, st *state.State,
	factory clientfactory.Factory, ios cli.IOStreams, isMove bool) error {
	// 卷上下文（URI / 当前工作目录所在卷 ⇒ --volume；用户口径 2026-10-13）。
	if err := applyVolumeContext(cmd, st, args); err != nil {
		return err
	}
	svc, err := factory.NewClient(cmd)
	if err != nil {
		return fmt.Errorf(errFmtInitClient, err)
	}
	fromVol, _ := cmd.Flags().GetString(flagFromVolume)
	if fromVol == "" {
		fromVol = svc.Volume()
	}
	if fromVol == "" {
		return fmt.Errorf("--from-volume 必填（当前无卷上下文）")
	}
	toVol, _ := cmd.Flags().GetString(flagToVolume)
	if toVol == "" {
		return fmt.Errorf("--to-volume 必填")
	}
	filename := args[0]
	if !isRemoteAbs(filename) {
		resolved, rerr := st.ResolveRemotePath(filename)
		if rerr != nil {
			return fmt.Errorf("解析路径失败: %w", rerr)
		}
		filename = resolved
	}
	verb, domainErr := "复制", svc.CopyVolume
	if isMove {
		verb, domainErr = "移动", svc.MoveVolume
	}
	if xerr := domainErr(cmd.Context(), fromVol, toVol, filename); xerr != nil {
		ios.WriteErrLine("跨卷%s失败: %v", verb, xerr)
		return fmt.Errorf("跨卷%s失败: %w", verb, xerr)
	}
	ios.WriteOutLine("%s完成: %s（%s → %s）", verb, filename, fromVol, toVol)
	return nil
}
