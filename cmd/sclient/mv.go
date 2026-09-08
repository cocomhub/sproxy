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

// ---- 工厂函数 ----

// NewCmdMv 创建独立的 mv 命令工厂函数，使用 state.State 替代全局 currentDir。
//
// 卷语义：
//   - 缺省（不加 --volume / --to-volume）＝ auto：与旧版完全一致——跨卷定位源文件，
//     rename 在源 home 卷内完成；
//   - --volume <卷>：把源限定到指定卷（同卷 rename）；
//   - --to-volume <卷>：把文件迁移到目标卷（同相对路径；跨卷写前查重）。源与目标同卷时
//     退化为同卷 rename；跨卷时先调 move API 再按需 rename（改名）。
func NewCmdMv(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mv <from> <to>",
		Short: "重命名 / 移动远端文件",
		Long: `重命名或移动 sproxy 服务端上的文件。

		服务端会先校验源文件的 SHA-256（避免在并发写入下误覆盖），然后执行 rename。
		目标父目录不存在时自动 mkdir -p；目标已存在时返回 409。

		from 和 to 都受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		--to-volume <卷> 把文件迁移到目标卷（跨卷 move API；目标卷已存在同 rel 时服务端 409）。
		未指定 --volume 时源卷按当前文件实际所在卷自动判定。

		示例:
		  sclient mv old.txt new.txt
		  sclient mv old.txt sub/dir/new.txt
		  sclient mv /a/b.txt /c/b.txt
		  sclient mv a.txt a.txt --to-volume disk2       # 跨卷迁移到 disk2（同路径）
		  sclient mv --volume main a.txt b.txt --to-volume disk2  # main→disk2 迁移并改名`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			from, err := st.ResolveRemotePathOrErr(args[0])
			if err != nil {
				return err
			}
			to, err := st.ResolveRemotePathOrErr(args[1])
			if err != nil {
				return err
			}

			toVol, _ := cmd.Flags().GetString("to-volume")
			ctx := cmd.Context()

			info, err := svc.Stat(ctx, from)
			if err != nil {
				ios.WriteErrLine("获取源文件信息失败: %v", err)
				return fmt.Errorf("获取源文件信息失败: %w", err)
			}
			if info.Checksum == "" {
				return fmt.Errorf("源文件 checksum 为空，无法重命名")
			}

			// 无 --to-volume：同卷 rename（--volume 提供源卷上下文时限定到该卷）。
			if toVol == "" {
				if err = svc.Rename(ctx, from, to, info.Checksum); err != nil {
					ios.WriteErrLine("重命名失败: %v", err)
					return fmt.Errorf("重命名失败: %w", err)
				}
				fmt.Fprintf(ios.Out, "已重命名: %s -> %s\n", from, to)
				return nil
			}

			// --to-volume：判定源 home 卷（--volume 显式给定时用它；否则按当前文件实际位置）。
			home := svc.Volume()
			if home == "" {
				home, err = svc.VolumeOf(ctx, from)
				if err != nil {
					ios.WriteErrLine("定位源文件所在卷失败: %v", err)
					return fmt.Errorf("定位源文件所在卷失败: %w", err)
				}
			}
			if home == toVol {
				// 源已在目标卷：同卷 rename。
				if err = svc.Rename(ctx, from, to, info.Checksum); err != nil {
					ios.WriteErrLine("重命名失败: %v", err)
					return fmt.Errorf("重命名失败: %w", err)
				}
				fmt.Fprintf(ios.Out, "已重命名: %s -> %s\n", from, to)
				return nil
			}

			// 跨卷：先 move（同 rel），再按需 rename 到目标名。
			if err = svc.MoveVolume(ctx, home, toVol, from); err != nil {
				ios.WriteErrLine("跨卷移动失败: %v", err)
				return fmt.Errorf("跨卷移动失败: %w", err)
			}
			if to != from {
				svc.SetVolume(toVol)
				if err := svc.Rename(ctx, from, to, info.Checksum); err != nil {
					ios.WriteErrLine("跨卷移动成功但重命名失败（文件已位于 %s 卷的 %s）: %v", toVol, from, err)
					return fmt.Errorf("跨卷移动成功但重命名失败: %w", err)
				}
				fmt.Fprintf(ios.Out, "已移动: %s (%s → %s) 并重命名为: %s\n", from, home, toVol, to)
				return nil
			}
			fmt.Fprintf(ios.Out, "已移动: %s (%s → %s)\n", from, home, toVol)
			return nil
		},
	}
	cmd.Flags().String("to-volume", "", "目标卷：跨卷迁移到该卷（缺省 = 同卷 rename）")
	return cmd
}
