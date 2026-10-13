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

// NewCmdCp 创建 cp 命令（常规复制语义，**源保留**）：
//   - 同卷：`cp a.txt b.txt`（默认 = 与源同卷；卷支持时走**底层 blob 直拷**，不重新加解密）；
//   - 跨卷：`cp a.txt b.txt --to-volume disk2` 或**卷 URI** `cp main:/a.txt disk2:/a.txt`；
//   - URI 寻址与工作目录规则同其它文件命令（见 docs/cli.md）。
func NewCmdCp(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp <src> <dst> [flags]",
		Short: "复制文件（支持同卷 / 跨卷 / 卷 URI）",
		Long: `复制文件，**源保留**（与 mv 相对）。

路径支持卷 URI（<卷名>:/<路径>），也受当前工作目录（含所在卷）影响：
  sclient cp a.txt b.txt                     # 同卷复制（目标与源同卷）
  sclient cp a.txt b.txt --to-volume disk2    # 跨卷复制到 disk2（同相对路径）
  sclient cp vault:/a.txt vault:/b.txt        # 卷 URI（同卷）
  sclient cp main:/a.txt vault:/a.txt         # 跨卷（两端 URI）

默认**不覆盖**已存在的目标（409）；显式 --force 才覆盖（先删后写，非原子）。
同卷且卷支持「底层 blob 直拷」（如 secretdata 加密卷）时不重新加解密。`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷上下文：源 URI → --volume；目标 URI（不同卷）→ --to-volume（用户口径 2026-10-13）。
			if err := applyMvVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			from, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				ios.WriteErrLine("无效的源路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}
			to, err := st.ResolveRemotePathOrErr(args[1])
			if err != nil {
				ios.WriteErrLine("无效的目标路径: %v", err)
				return fmt.Errorf(errFmtInvalidPath, err)
			}
			toVol, _ := cmd.Flags().GetString(flagToVolume)
			force, _ := cmd.Flags().GetBool(flagForce)
			if err := svc.CopyFile(cmd.Context(), svc.Volume(), from, toVol, to, force); err != nil {
				ios.WriteErrLine("复制失败: %v", err)
				return fmt.Errorf("复制失败: %w", err)
			}
			ios.WriteOutLine("复制完成: %s → %s", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().Bool(flagForce, false, "覆盖已存在的目标（默认拒绝，绝不静默覆盖）")
	cmd.Flags().String(flagToVolume, "", "目标卷（缺省 = 与源同卷）")
	return cmd
}
