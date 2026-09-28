// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdBackup 创建 backup 子命令：把卷导出为 tar 备份到本地（roadmap 11.7-5）。
//
//	sclient backup <vol> <dest>
//
// 服务端 GET /api/volumes/export（tar 流式 + 尾部 manifest.json）已就绪（#602）——
// 本命令只做 CLI 封装：调 ExportVolume → 流式写本地 dest。
// 卷名为空 = 全卷视图（服务端导出 owner 全部可见卷）。
//
// 定时备份（roadmap 12.2-3 P2）：加 --schedule <cron> 后按 cron 表达式周期触发
// （参考 #561 sync schedule 模式；到点执行一次导出，串行不堆叠，SIGINT/SIGTERM 优雅退出）。
func NewCmdBackup(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup <vol> <dest>",
		Short: "导出卷为 tar 备份到本地",
		Long: `把卷导出为 tar 备份到本地文件（服务端 GET /api/volumes/export 流式导出）。

	<vol> 指定卷名（留空 = 导出当前凭据可见的全部卷）；
	<dest> 为本地目标 .tar 文件路径（父目录不存在时自动创建；原子落盘）。

	导出 tar 含全部文件内容 + 尾部 manifest.json（每条目相对路径 + SHA-256 + size +
	mtime + 台账交叉校验），可用于跨实例迁移 / 恢复（服务端 POST /api/volumes/import）。

	--schedule 指定 cron 表达式（分 时 日 月 周，同系统 crontab）时进入定时模式：
	到点触发一次导出（串行，不堆叠），Ctrl-C/SIGTERM 在当前导出完成后退出。
	示例：
	  sclient backup disk2 /backup/disk2.tar --schedule "30 2 * * *"   # 每天 02:30`,
		Example: `  sclient backup disk2 backup-disk2.tar
  sclient backup "" all-volumes.tar   # 导出全部可见卷
  sclient backup disk2 -o /backup/disk2.tar
  sclient backup disk2 /backup/disk2.tar --schedule "30 2 * * *"   # 定时每天 02:30`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine(errFmtInitClientPrint, err)
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

			schedule, _ := cmd.Flags().GetString("schedule")
			if strings.TrimSpace(schedule) != "" {
				return runScheduledBackup(cmd, ios, svc, vol, dest, schedule)
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
	cmd.Flags().String("schedule", "", "定时备份：cron 表达式（分 时 日 月 周）到点触发一次导出（空 = 单次导出）")
	return cmd
}

// runScheduledBackup 按 cron 表达式周期触发备份（参考 #561 sync schedule 模式）：
// 到点串行执行一次导出（不堆叠）；SIGINT/SIGTERM 优雅退出（当前导出完成后停止）。
// cron 解析失败 / 未来 1 年无命中时刻 → 报错（fail-closed，不静默单次）。
func runScheduledBackup(cmd *cobra.Command, ios cli.IOStreams, svc *client.FileClient, vol, dest, spec string) error {
	expr, err := parseCronExpr(spec)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	next, nerr := expr.nextAfter(time.Now())
	if nerr != nil {
		return nerr
	}
	volTxt := vol
	if volTxt == "" {
		volTxt = "<全部卷>"
	}
	ios.WriteOutLine("backup schedule: %s 下次触发 %s（卷 %s → %s）",
		spec, next.Format(time.RFC3339), volTxt, dest)
	return backupScheduleLoop(ctx, expr, svc, vol, dest, volTxt, next, ios)
}

// backupScheduleLoop 定时备份主循环：到点串行执行一次导出（不堆叠），完成后计算
// 下一次触发时刻；睡到下一个触发时刻（最长 30s）并响应取消。到点导出失败：记错后
// 继续等待下一次（串行调度不退出，与 sync schedule 同语义）。
func backupScheduleLoop(ctx context.Context, expr *cronExpr, svc *client.FileClient, vol, dest, volTxt string, next time.Time, ios cli.IOStreams) error {
	var nerr error
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := time.Now()
		next, nerr = backupRunDue(ctx, expr, svc, vol, dest, volTxt, now, next, ios)
		if nerr != nil {
			return nerr
		}
		// 睡到下一个触发时刻（最长 30s），响应取消。
		wait := min(time.Until(next), 30*time.Second)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// backupRunDue 到点（!now.Before(next)）时执行一次导出并计算下一次触发时刻；
// 未到点时原样返回 next。导出失败记错后不退出，与 sync schedule 同语义。
func backupRunDue(ctx context.Context, expr *cronExpr, svc *client.FileClient, vol, dest, volTxt string, now, next time.Time, ios cli.IOStreams) (time.Time, error) {
	if now.Before(next) {
		return next, nil
	}
	if err := svc.ExportVolume(ctx, vol, dest); err != nil {
		// 到点失败：记错后继续等待下一次（串行调度不退出）。
		if ctx.Err() != nil {
			return next, nil
		}
		ios.WriteErrLine("backup schedule: 导出失败: %v", err)
	} else {
		ios.WriteOutLine("备份完成: %s（卷 %s）", dest, volTxt)
	}
	next, nerr := expr.nextAfter(now)
	if nerr != nil {
		return next, nerr
	}
	if ctx.Err() == nil {
		ios.WriteOutLine("backup schedule: 下次触发 %s", next.Format(time.RFC3339))
	}
	return next, nil
}
