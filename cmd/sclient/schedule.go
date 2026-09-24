// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// schedule.go 实现 `sync schedule <cron>` 子命令（roadmap P2 定时调度同步残余）：
// 标准库实现的 5 字段 cron 表达式解析（分 时 日 月 周）+ 到点触发一次服务端同步
// （复用 sync watch 的 triggerOneSync：CreateSyncTask 服务端执行，客户端只调度）。
// 零新依赖（不引 robfig/cron）——字段语义与系统 crontab 对齐。

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
)

// cronExpr 是解析后的 cron 表达式（5 字段：分 时 日 月 周）。
type cronExpr struct {
	minute, hour, dom, month, dow []int // 各字段允许值集合（0/1 基：minute 0-59、hour 0-23、dom 1-31、month 1-12、dow 0-6 且 0=周日）
}

// parseCronExpr 解析 "分 时 日 月 周"（支持 *、数值、逗号列表、- 区间）。
func parseCronExpr(spec string) (*cronExpr, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式需 5 字段（分 时 日 月 周），got %d", len(fields))
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	expr := &cronExpr{}
	for i, f := range fields {
		vals, err := parseCronField(f, ranges[i][0], ranges[i][1])
		if err != nil {
			return nil, fmt.Errorf("cron 字段 %d（%q）: %w", i+1, f, err)
		}
		switch i {
		case 0:
			expr.minute = vals
		case 1:
			expr.hour = vals
		case 2:
			expr.dom = vals
		case 3:
			expr.month = vals
		case 4:
			expr.dow = vals
		}
	}
	return expr, nil
}

// parseCronField 解析单字段（*、数值、逗号列表、a-b 区间）。
func parseCronField(f string, min, max int) ([]int, error) {
	var out []int
	for part := range strings.SplitSeq(f, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			for v := min; v <= max; v++ {
				out = append(out, v)
			}
			continue
		}
		// */N 步长（如 */6 → 0,6,12,...）。
		if after, ok := strings.CutPrefix(part, "*/"); ok {
			step, err := strconv.Atoi(after)
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("非法步长 %q", part)
			}
			for v := min; v <= max; v += step {
				out = append(out, v)
			}
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			lo, err1 := strconv.Atoi(bounds[0])
			hi, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("非法区间 %q", part)
			}
			if lo < min || hi > max || lo > hi {
				return nil, fmt.Errorf("区间 %q 越界 [%d,%d]", part, min, max)
			}
			for v := lo; v <= hi; v++ {
				out = append(out, v)
			}
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil || v < min || v > max {
			return nil, fmt.Errorf("非法数值 %q（范围 [%d,%d]）", part, min, max)
		}
		out = append(out, v)
	}
	return out, nil
}

// matches 判断 t 是否命中表达式（日/月双匹配：dom 与 dow 任一命中即真，同 crontab）。
func (e *cronExpr) matches(t time.Time) bool {
	domOK := intIn(e.dom, t.Day())
	dowOK := intIn(e.dow, int(t.Weekday())) // Weekday: Sunday=0
	// 标准 crontab 语义：dom 与 dow 任一命中即真（两者任一受限都按 OR）。
	return intIn(e.minute, t.Minute()) && intIn(e.hour, t.Hour()) &&
		intIn(e.month, int(t.Month())) && (domOK || dowOK)
}

func intIn(list []int, v int) bool {
	return slices.Contains(list, v)
}

// nextAfter 返回 t 之后（严格大于）的下一个命中时刻（每分钟检查，1 年内找不到报错）。
func (e *cronExpr) nextAfter(t time.Time) (time.Time, error) {
	for range 366 * 24 * 60 {
		t = t.Add(time.Minute)
		if e.matches(t) {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cron 表达式在未来 1 年内无命中时刻")
}

// newCmdSyncSchedule 创建 `sync schedule <cron>` 子命令：cron 表达式到点触发同步。
func newCmdSyncSchedule(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	var o syncWatchOptions
	cmd := &cobra.Command{
		Use:   "schedule <cron>",
		Short: "定时调度同步：cron 表达式到点触发 pull/push/both",
		Long: `按 cron 表达式（分 时 日 月 周，同系统 crontab）周期触发一次服务端同步任务。

示例：
  sync schedule "0 */6 * * *" --direction pull          # 每 6 小时整点拉取
  sync schedule "30 2 * * *"  --direction both          # 每天 02:30 双向
  sync schedule "*/15 * * * *" --direction push         # 每 15 分钟推送

与 sync watch 互补：watch 是事件驱动连续同步，schedule 是时间表驱动定时同步。
到点触发走服务端 SyncManager（CreateSyncTask + waitSyncTask，与 watch 同语义）。`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			expr, err := parseCronExpr(args[0])
			if err != nil {
				return err
			}
			if o.remote == "" {
				return fmt.Errorf("--remote 必填（服务端 sync_remotes 配置的远程节点名）")
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			runner := &syncWatcher{
				svc: svc, ios: ios,
				remote: o.remote, src: o.src, dst: o.dst,
				verify: o.verify, quiet: o.quiet,
				direction:       o.direction,
				deletePropagate: o.deletePropagate,
			}
			if runner.direction == "" {
				runner.direction = "pull"
			}
			// 首次调度：打印下一个触发时刻。
			next, nerr := expr.nextAfter(time.Now())
			if nerr != nil {
				return nerr
			}
			if !o.quiet {
				ios.WriteOutLine("schedule: %s 下次触发 %s（direction=%s）", args[0], next.Format(time.RFC3339), runner.direction)
			}
			// 主循环：到点触发一次（串行），完成后计算下一次。
			for {
				if ctx.Err() != nil {
					return nil
				}
				now := time.Now()
				if !now.Before(next) {
					if err := runner.triggerSync(ctx); err != nil && ctx.Err() == nil {
						ios.WriteErrLine("schedule: 同步任务失败: %v", err)
					}
					next, nerr = expr.nextAfter(now)
					if nerr != nil {
						return nerr
					}
					if !o.quiet {
						ios.WriteOutLine("schedule: 下次触发 %s", next.Format(time.RFC3339))
					}
				}
				// 睡到下一个整分钟边界（最长 30s），响应取消。
				wait := min(time.Until(next), 30*time.Second)
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		},
	}
	cmd.Flags().StringVar(&o.remote, "remote", "", "远程节点名（服务端 sync_remotes 配置名，必填）")
	cmd.Flags().StringVar(&o.src, "src", "", "源路径（push=本地相对路径；pull=远程相对路径；默认 \"\" = 根）")
	cmd.Flags().StringVar(&o.dst, "dst", "", "目标路径（push=远程相对路径；pull=本地相对路径；默认 \"\" = 根）")
	cmd.Flags().BoolVar(&o.verify, "verify", false, "每次同步完成后校验核对")
	cmd.Flags().BoolVar(&o.quiet, "quiet", false, "静默模式：不打印每次同步明细")
	cmd.Flags().StringVar(&o.direction, "direction", "pull", "触发方向（pull|push|both；默认 pull 零回归）")
	cmd.Flags().BoolVar(&o.deletePropagate, "delete-propagate", false, "push 方向传播 delete 事件（默认跳过防误删远程）")
	return cmd
}
