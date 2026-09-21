// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// syncWatchOptions 是 sync watch 子命令的 flag 集合。
type syncWatchOptions struct {
	remote      string
	src         string
	dst         string
	verify      bool
	pollSeconds int
	debounceMs  int
	quiet       bool
}

// newCmdSyncWatch 创建 `sync watch` 子命令：订阅本地服务端 /api/events 事件流，
// 文件变更事件到达 → 触发一次服务端 pull 同步任务（从远程拉到本地），实现连续同步。
//
// 设计决策（对齐 sync push/pull 的 SyncManager 托管模型）：
//   - 事件源是本地 sproxy 的事件流（#433/#437）；watch 命令只负责「订阅 + 触发 + 等待」，
//     实际同步仍由服务端 SyncManager 执行（复用 CreateSyncTask + waitSyncTask，不引入
//     客户端本地引擎）——与服务端任务语义/审计/统计完全一致。
//   - 事件动作过滤：upload/rename/delete/mkdir/rmdir/version 才触发（share 等元事件忽略）。
//   - 去抖：同一窗口内（默认 500ms）的连续事件合并为一次同步，避免高频变更刷屏任务。
//   - 事件流不可用（认证失败/网络断）→ 退化 --poll 间隔轮询（默认 30s，显式可配），
//     退化必须日志告警（禁静默丢事件）。
//   - SIGINT/SIGTERM 优雅退出：当前同步任务完成后退出（signal.NotifyContext）。
func newCmdSyncWatch(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	var o syncWatchOptions

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "连续同步：事件流驱动增量 pull（替代轮询）",
		Long: `订阅本地 sproxy 的文件变更事件流（/api/events），每次变更触发一次 pull 同步任务，
把远程节点最新变更拉到本地——实现「变更秒级传播、无变更零开销」的连续同步。

事件流不可用（认证失败/断网）时自动退化 --poll 间隔轮询（默认 30s，显式可配），
退化会打印日志告警（不会静默丢事件）。SIGINT/Ctrl-C 优雅退出（当前任务完成后）。

--remote/--src/--dst 语义与 sync pull 一致；--verify 在每次同步后校验核对。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.remote == "" {
				return fmt.Errorf("--remote 必填（服务端 sync_remotes 配置的远程节点名）")
			}
			if o.pollSeconds <= 0 {
				return fmt.Errorf("--poll 必须 > 0 秒（轮询回退间隔）")
			}
			if o.debounceMs <= 0 {
				return fmt.Errorf("--debounce-ms 必须 > 0（去抖窗口毫秒）")
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			watcher := &syncWatcher{
				svc:        svc,
				ios:        ios,
				remote:     o.remote,
				src:        o.src,
				dst:        o.dst,
				verify:     o.verify,
				poll:       time.Duration(o.pollSeconds) * time.Second,
				debounce:   time.Duration(o.debounceMs) * time.Millisecond,
				quiet:      o.quiet,
				lastCursor: 0,
			}
			return watcher.run(ctx)
		},
	}

	cmd.Flags().StringVar(&o.remote, "remote", "", "远程节点名（服务端 sync_remotes 配置名，必填）")
	cmd.Flags().StringVar(&o.src, "src", "", "源路径（pull 的远程相对路径；默认 \"\" = 远程根）")
	cmd.Flags().StringVar(&o.dst, "dst", "", "目标路径（pull 的本地相对路径；默认 \"\" = 本地根）")
	cmd.Flags().BoolVar(&o.verify, "verify", false, "每次同步完成后校验核对（重读目标 checksum 与源比对）")
	cmd.Flags().IntVar(&o.pollSeconds, "poll", 30, "轮询回退间隔秒数（事件流不可用时）")
	cmd.Flags().IntVar(&o.debounceMs, "debounce-ms", 500, "事件去抖窗口毫秒（窗口内合并为一次同步）")
	cmd.Flags().BoolVar(&o.quiet, "quiet", false, "静默模式：不打印每次同步明细（仅事件/退化告警）")
	return cmd
}

// syncWatcher 是 watch 的运行主体：订阅事件流 + 触发同步 + 轮询回退。
type syncWatcher struct {
	svc        *client.FileClient
	ios        cli.IOStreams
	remote     string
	src        string
	dst        string
	verify     bool
	poll       time.Duration
	debounce   time.Duration
	quiet      bool
	lastCursor uint64
	// eventMode 标记当前是否为事件流模式（false = 已退化轮询）。
	eventMode bool
	// lastTrigger 上次触发同步的时刻（去抖窗口判定）。
	lastTrigger time.Time
	// degradedLogged 防重复打印退化告警。
	degradedLogged bool
}

// run 进入 watch 主循环：优先事件流，失败退化为轮询；SIGINT 优雅退出。
func (w *syncWatcher) run(ctx context.Context) error {
	if !w.quiet {
		w.ios.WriteOutLine("watch: 订阅 %s 的事件流（remote=%s）...", w.svc.ServerURL(), w.remote)
	}
	err := w.watchEvents(ctx)
	if err != nil {
		// 事件流不可用（认证失败/协议错误）→ 退化轮询。
		if !w.degradedLogged {
			w.ios.WriteErrLine("watch: 事件流不可用（%v），退化 %s 间隔轮询——变更传播有延迟", err, w.poll)
			w.degradedLogged = true
		}
		return w.watchPoll(ctx)
	}
	return nil
}

// watchEvents 订阅事件流；事件到达去抖后触发 pull 同步。
// 返回 nil = 正常退出（ctx 取消）；返回 error = 事件流不可用（调用方退化轮询）。
func (w *syncWatcher) watchEvents(ctx context.Context) error {
	w.eventMode = true
	return w.svc.WatchEvents(ctx, client.WatchEventsOptions{
		Owner:       "",
		LastEventID: w.lastCursor,
	}, func(ev client.FileEvent) {
		w.lastCursor = ev.Cursor
		if !isWatchAction(ev.Action) {
			return // 元事件（share 等）不触发同步
		}
		// 去抖窗口：同一窗口内（debounce）的连续事件只触发一次同步。
		now := time.Now()
		if !w.lastTrigger.IsZero() && now.Sub(w.lastTrigger) < w.debounce {
			if !w.quiet {
				w.ios.WriteOutLine("watch: 事件 %s %s 在去抖窗口内，合并待触发", ev.Action, ev.Rel)
			}
			return
		}
		w.lastTrigger = now
		if !w.quiet {
			w.ios.WriteOutLine("watch: 事件 %s %s（owner=%s）→ 触发增量同步", ev.Action, ev.Rel, ev.Owner)
		}
		if err := w.triggerSync(ctx); err != nil {
			w.ios.WriteErrLine("watch: 同步任务失败: %v", err)
		}
	})
}

// watchPoll 轮询模式：--poll 间隔主动拉取远程变更（事件流不可用的回退）。
func (w *syncWatcher) watchPoll(ctx context.Context) error {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	// 进入即触发一次（回退初期先同步一次，避免等待首个周期）。
	if err := w.triggerSync(ctx); err != nil && ctx.Err() == nil {
		w.ios.WriteErrLine("watch: 同步任务失败: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.triggerSync(ctx); err != nil && ctx.Err() == nil {
				w.ios.WriteErrLine("watch: 同步任务失败: %v", err)
			}
		}
	}
}

// triggerSync 触发一次服务端 pull 同步任务并等待完成（串行，避免任务堆积）。
func (w *syncWatcher) triggerSync(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	req := client.SyncTaskRequest{
		Direction:   "pull",
		Remote:      w.remote,
		Src:         w.src,
		Dst:         w.dst,
		VerifyAfter: w.verify,
	}
	task, err := w.svc.CreateSyncTask(ctx, req)
	if err != nil {
		return fmt.Errorf("创建同步任务失败: %w", err)
	}
	// 等待完成（对齐 sync pull --wait 语义；超时用 10 分钟防长任务挂起 watch）。
	if err := waitSyncTask(ctx, w.ios, w.svc, task.ID, 10*time.Minute, 2*time.Second, false); err != nil {
		return err
	}
	if !w.quiet {
		w.ios.WriteOutLine("watch: 同步完成 %s", task.ID)
	}
	return nil
}

// isWatchAction 判定事件动作是否触发同步（文件内容相关；share 等元事件忽略）。
func isWatchAction(action string) bool {
	switch action {
	case "upload", "rename", "delete", "mkdir", "rmdir", "version":
		return true
	default:
		return false
	}
}
