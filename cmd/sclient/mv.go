// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/volumectx"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- 工厂函数 ----

// mvCtx 是 mv 子命令的共享 CLI 上下文（S107 收敛：cmd/svc/ios 三件套在 mv 各分支反复
// 传递，抽成小结构体减少参数组）。
type mvCtx struct {
	cmd *cobra.Command
	svc *client.FileClient
	ios cli.IOStreams
}

// NewCmdMv 创建独立的 mv 命令工厂函数，使用 state.State 替代全局 currentDir。
//
// 卷语义（唯一寻址方式 = 路径内卷 URI，用户口径 2026-10-14）：
//   - 两侧都不写卷名 ⇒ auto：跨卷定位源文件，rename 在源 home 卷内完成；
//   - 源写 `卷:/路径` ⇒ 源限定到该卷（同卷 rename）；
//   - 目标写**不同**卷 ⇒ 跨卷迁移（move API；跨卷写前查重）；同卷 ⇒ 退化为同卷 rename。
func NewCmdMv(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mv <from> <to>",
		Short: "重命名 / 移动远端文件",
		Long: `重命名或移动 sproxy 服务端上的文件。

		服务端会先校验源文件的 SHA-256（避免在并发写入下误覆盖），然后执行 rename。
		目标父目录不存在时自动 mkdir -p；目标已存在时返回 409。

		from 和 to 都受当前目录 (cd) 影响：相对路径自动拼接前缀，绝对路径 (/开头) 绕过。

		卷寻址：路径写 '<卷名>:/<路径>'（唯一方式，无 --volume flag）。两侧**不同卷** = 跨卷迁移
		（服务端 move API；目标卷已存在同 rel 时 409）；同卷 = 改名。未写卷名时按当前工作目录
		所在卷（'cd <卷名>:/…'）或 auto 判定。

		示例:
		  sclient mv old.txt new.txt
		  sclient mv old.txt sub/dir/new.txt
		  sclient mv /a/b.txt /c/b.txt
		  sclient mv disk1:/a.txt disk2:/a.txt            # 跨卷迁移到 disk2（同路径）
		  sclient mv disk1:/a.txt disk2:/b.txt            # 跨卷迁移并改名`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 卷 URI 寻址：源 `A:/x` ⇒ 源卷上下文 A；目标 `B:/y`（不同卷）⇒ 目标卷上下文 B。
			if err := applyMvVolumeContext(cmd, st, args); err != nil {
				return err
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			return runCmdMv(cmd, svc, ios, st, args)
		},
	}
	return cmd
}

// runCmdMv 执行 mv 命令主体（NewCmdMv 的 RunE 抽出，降 CC）：
// 解析 from/to（受 cd 影响）→ 校验源 checksum → 同卷 rename 或跨卷迁移
// （--to-volume；源已在该卷时退化为同卷 rename）。
func runCmdMv(cmd *cobra.Command, svc *client.FileClient, ios cli.IOStreams, st *state.State, args []string) error {
	from, err := st.ResolveRemotePathOrErr(args[0])
	if err != nil {
		return err
	}
	to, err := st.ResolveRemotePathOrErr(args[1])
	if err != nil {
		return err
	}
	toVol, _ := volumectx.Target(cmd.Context())
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
	mc := mvCtx{cmd: cmd, svc: svc, ios: ios}
	if toVol == "" {
		return mvSameVolumeRename(mc, from, to, info.Checksum)
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
		return mvSameVolumeRename(mc, from, to, info.Checksum)
	}
	return mvCrossVolume(mc, from, to, home, toVol, info.Checksum)
}

// mvSameVolumeRename 同卷 rename 并打印结果。
func mvSameVolumeRename(mc mvCtx, from, to, checksum string) error {
	if err := mc.svc.Rename(mc.cmd.Context(), from, to, checksum); err != nil {
		mc.ios.WriteErrLine("重命名失败: %v", err)
		return fmt.Errorf("重命名失败: %w", err)
	}
	fmt.Fprintf(mc.ios.Out, "已重命名: %s -> %s\n", from, to)
	return nil
}

// mvCrossVolume 跨卷迁移：先 move（同 rel），再按需 rename 到目标名。
func mvCrossVolume(mc mvCtx, from, to, home, toVol, checksum string) error {
	ctx := mc.cmd.Context()
	if err := mc.svc.MoveVolume(ctx, home, toVol, from); err != nil {
		mc.ios.WriteErrLine("跨卷移动失败: %v", err)
		return fmt.Errorf("跨卷移动失败: %w", err)
	}
	if to != from {
		mc.svc.SetVolume(toVol)
		if err := mc.svc.Rename(ctx, from, to, checksum); err != nil {
			mc.ios.WriteErrLine("跨卷移动成功但重命名失败（文件已位于 %s 卷的 %s）: %v", toVol, from, err)
			return fmt.Errorf("跨卷移动成功但重命名失败: %w", err)
		}
		fmt.Fprintf(mc.ios.Out, "已移动: %s (%s → %s) 并重命名为: %s\n", from, home, toVol, to)
		return nil
	}
	fmt.Fprintf(mc.ios.Out, "已移动: %s (%s → %s)\n", from, home, toVol)
	return nil
}
