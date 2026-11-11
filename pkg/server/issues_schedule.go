// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// issues_schedule.go 把「待处理事项」的**定期重试**接入统一调度器（`pkg/server/scheduler`）。
//
// 用户裁定 2026-11-10：「记录失败事件审计重试能力，**后续定期重试处理，最终必须成功**」。
// 机制侧（`issues.Manager.RunDue`）已实现「指数退避 + 有界 + 达上限转人工 + 无执行器转人工」；
// 本文件只负责**周期触发**，闭环为：
//
//	登记事项 → 每 5m 触发一次到期重试 → 成功解除 / 失败退避 / 有界后转人工
//
// 设计约定：
//   - 间隔与首次退避一致（5m），避免"调度比退避更密"造成的无效空转；
//   - 任务**不因单条失败而返回 error**：单条失败已由 `Manager` 记录（`LastError`/`Attempts`）并按
//     退避继续；把整个周期任务标记为失败只会污染调度器指标，不带来额外信息；
//   - 未装配调度器/事项中枢 ⇒ 直接返回 nil（零回归）。

import (
	"context"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/server/scheduler"
)

// issuesRetryInterval 是待处理事项的定期重试间隔（与 Manager 的首次退避一致）。
const issuesRetryInterval = 5 * time.Minute

// IssueCenter 返回事项中枢（未装配 ⇒ nil）。
func (h *Handlers) IssueCenter() *IssueCenter {
	if h == nil || h.issuesAPI == nil {
		return nil
	}
	center, _ := h.issuesAPI.snapshot()
	return center
}

// RegisterIssuesRetryTask 把「定期重试待处理事项」注册到调度器。
//
// executors 来自 `NewIssuesExecutors`（按事项类型分流执行器；未覆盖的类型由 Manager 转人工）。
// 调度器或中枢未装配 ⇒ 返回 nil（未启用，零回归）。
func (h *Handlers) RegisterIssuesRetryTask(executors map[issues.Kind]issues.RetryFunc) error {
	if h == nil || h.scheduler == nil {
		return nil
	}
	center := h.IssueCenter()
	if center == nil {
		return nil
	}
	log := h.log()
	return h.scheduler.Register(scheduler.Task{
		Name:     "issues-retry",
		Interval: issuesRetryInterval,
		Run: func(ctx context.Context) {
			res := center.RunIssuesOnce(ctx, time.Now(), executors)
			if res.Ran == 0 && res.Deferred == 0 {
				return // 无到期事项：静默（避免每 5m 刷日志）
			}
			log.Info("待处理事项重试完成",
				"ran", res.Ran, "succeeded", res.Succeeded, "failed", res.Failed, "deferred", res.Deferred)
		},
	})
}
