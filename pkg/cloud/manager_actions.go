// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_actions.go 是**失败任务的人工处理动作**（用户 2026-11-26）。
//
// 背景：失败任务此前只有「重试」（等价全量重跑）与「取消」两种出路——下载阶段失败但已下
// 大半的任务被迫重下；转存阶段失败的任务又被要求重下（明明产物已完整）。
// 这里把恢复拆成四种可组合意图，全部复用 resumeTaskWith 内核（不新增执行路径）：
//
//	retry_download   退回下载阶段**续传**（保留 .partial / hybrid manifest）
//	retry_transfer   仅重做转存（产物完整，跳过下载）
//	rotate_source    换下一个备选源并重下
//	discard_partial  清残留（产物 + .partial）后从零重下
package cloud

import "fmt"

// TaskAction 是失败任务的人工处理动作。
type TaskAction string

const (
	// TaskActionRetryDownload 退回下载阶段续传（保留已下载部分）。
	TaskActionRetryDownload TaskAction = "retry_download"
	// TaskActionRetryTransfer 仅重做转存（要求产物完整且配置了转存目标）。
	TaskActionRetryTransfer TaskAction = "retry_transfer"
	// TaskActionRotateSource 换下一个备选源重下（要求有 FallbackSources）。
	TaskActionRotateSource TaskAction = "rotate_source"
	// TaskActionDiscardPartial 清残留（产物 + .partial）后从零重下。
	TaskActionDiscardPartial TaskAction = "discard_partial"
)

// ParseTaskAction 解析动作名（未知动作明确报错，便于 CLI/HTTP 快速失败）。
func ParseTaskAction(s string) (TaskAction, error) {
	switch TaskAction(s) {
	case TaskActionRetryDownload, TaskActionRetryTransfer, TaskActionRotateSource, TaskActionDiscardPartial:
		return TaskAction(s), nil
	default:
		return "", fmt.Errorf("未知任务动作 %q（可用: retry_download | retry_transfer | rotate_source | discard_partial）", s)
	}
}

// ApplyTaskAction 对失败/取消任务执行细粒度人工处理，返回动作提交后的任务快照。
//
// 只接受 failed/cancelled（以及 completed+damaged 走 retry_download）——其余状态由
// resumeTaskWith 的既有校验拒绝（错误文案与 ResumeTask 一致）。
func (m *CloudDownloadManager) ApplyTaskAction(taskID string, action TaskAction, owner string) (*CloudTask, error) {
	snap, ok := m.SnapshotTask(taskID, owner)
	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	opts, err := actionResumeOptions(action, snap)
	if err != nil {
		return nil, err
	}
	if err := m.resumeTaskWith(taskID, opts, owner); err != nil {
		return nil, err
	}
	if cur, ok := m.SnapshotTask(taskID, owner); ok {
		return cur, nil
	}
	return nil, fmt.Errorf("task not found after action: %s", taskID)
}

// actionResumeOptions 把动作映射为恢复选项（含前置条件校验）。
func actionResumeOptions(action TaskAction, t *CloudTask) (resumeOptions, error) {
	if t == nil {
		return resumeOptions{}, fmt.Errorf("task not found")
	}
	switch action {
	case TaskActionRetryDownload:
		return resumeOptions{}, nil
	case TaskActionRetryTransfer:
		if !t.DownloadDone {
			return resumeOptions{}, fmt.Errorf("下载尚未完成，无法只重做转存（请用 retry_download 续传）")
		}
		if t.Transfer == nil {
			return resumeOptions{}, fmt.Errorf("任务没有转存目标，无法重做转存")
		}
		return resumeOptions{keepDownloadDone: true}, nil
	case TaskActionRotateSource:
		if len(t.FallbackSources) < 2 {
			return resumeOptions{}, fmt.Errorf("没有备选源可切换（任务未提供 fallback_urls）")
		}
		return resumeOptions{rotateSource: true}, nil
	case TaskActionDiscardPartial:
		return resumeOptions{force: true}, nil
	default:
		return resumeOptions{}, fmt.Errorf("未知任务动作 %q", action)
	}
}

// TaskActionPlan 描述某任务**当前可用**的动作（供 v2 页面渲染操作菜单）。
type TaskActionPlan struct {
	Action   TaskAction `json:"action"`
	Label    string     `json:"label"`
	Hint     string     `json:"hint"`
	Disabled bool       `json:"disabled,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

// PlanTaskActions 返回任务的可用动作清单（failed/cancelled 才有动作；其他状态返回空）。
//
// 为什么放在服务端：可用性判据（产物是否完整/是否有备选源）只有服务端知道，
// 前端不应重复推断——否则会渲染出必然失败（或语义被吞掉）的按钮。
func PlanTaskActions(t *CloudTask) []TaskActionPlan {
	if t == nil || (t.Status != "failed" && t.Status != "cancelled") {
		return nil
	}
	plan := []TaskActionPlan{
		{Action: TaskActionRetryDownload, Label: "续传下载", Hint: "保留已下载部分，从断点继续（不重下已完成块）"},
	}
	transfer := TaskActionPlan{Action: TaskActionRetryTransfer, Label: "仅重做转存", Hint: "产物已完整，只重跑转存阶段"}
	if !t.DownloadDone {
		transfer.Disabled = true
		transfer.Reason = "下载未完成"
	} else if t.Transfer == nil {
		transfer.Disabled = true
		transfer.Reason = "无转存目标"
	}
	plan = append(plan, transfer)

	rotate := TaskActionPlan{Action: TaskActionRotateSource, Label: "换源重试", Hint: "切换到下一个备选源重新下载"}
	if len(t.FallbackSources) < 2 {
		rotate.Disabled = true
		rotate.Reason = "未提供备选源"
	}
	plan = append(plan, rotate)

	plan = append(plan, TaskActionPlan{
		Action: TaskActionDiscardPartial, Label: "清残留重下", Hint: "删除已有产物与断点，从零重新下载",
	})
	return plan
}
