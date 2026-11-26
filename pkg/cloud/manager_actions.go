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

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

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
	// TaskActionRetryTransferOverwrite 仅重做转存并**允许覆盖**已存在的目标（显式 opt-in）。
	// 用户 2026-11-26：目标冲突时此前只能手动删文件再重试。
	TaskActionRetryTransferOverwrite TaskAction = "retry_transfer_overwrite"
	// TaskActionRetryFromURL 用**新的源 URL** 重试（分享失效后换链接），保留已有断点。
	TaskActionRetryFromURL TaskAction = "retry_from_url"
)

// ParseTaskAction 解析动作名（未知动作明确报错，便于 CLI/HTTP 快速失败）。
func ParseTaskAction(s string) (TaskAction, error) {
	switch TaskAction(s) {
	case TaskActionRetryDownload, TaskActionRetryTransfer, TaskActionRotateSource, TaskActionDiscardPartial,
		TaskActionRetryTransferOverwrite, TaskActionRetryFromURL:
		return TaskAction(s), nil
	default:
		return "", fmt.Errorf("未知任务动作 %q（可用: retry_download | retry_transfer | retry_transfer_overwrite | "+
			"rotate_source | retry_from_url | discard_partial）", s)
	}
}

// ApplyTaskAction 对失败/取消任务执行细粒度人工处理，返回动作提交后的任务快照。
//
// 只接受 failed/cancelled（以及 completed+damaged 走 retry_download）——其余状态由
// resumeTaskWith 的既有校验拒绝（错误文案与 ResumeTask 一致）。
func (m *CloudDownloadManager) ApplyTaskAction(taskID string, action TaskAction, owner string) (*CloudTask, error) {
	return m.ApplyTaskActionRequest(taskID, TaskActionRequest{Action: action}, owner)
}

// TaskActionRequest 是动作请求（URL 仅 `retry_from_url` 使用；已由装配层做 SSRF/文件名校验）。
type TaskActionRequest struct {
	Action TaskAction `json:"action"`
	URL    string     `json:"url,omitempty"`
}

// ApplyTaskActionRequest 执行动作（支持需要额外参数的 `retry_from_url`）。
func (m *CloudDownloadManager) ApplyTaskActionRequest(taskID string, req TaskActionRequest, owner string) (*CloudTask, error) {
	action := req.Action
	snap, ok := m.SnapshotTask(taskID, owner)
	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	opts, err := actionResumeOptions(req, snap)
	if err == nil {
		err = m.verifyActionPreconditions(action, snap)
	}
	if err != nil {
		m.metrics.TaskActions.Inc(string(action), "error")
		return nil, err
	}
	if merr := m.mutateTaskForAction(taskID, req, owner); merr != nil {
		m.metrics.TaskActions.Inc(string(action), "error")
		return nil, merr
	}
	if rerr := m.resumeTaskWith(taskID, opts, owner); rerr != nil {
		m.metrics.TaskActions.Inc(string(action), "error")
		return nil, rerr
	}
	m.metrics.TaskActions.Inc(string(action), "success")
	if cur, ok := m.SnapshotTask(taskID, owner); ok {
		return cur, nil
	}
	return nil, fmt.Errorf("task not found after action: %s", taskID)
}

// actionResumeOptions 把动作映射为恢复选项（含前置条件校验）。
func actionResumeOptions(req TaskActionRequest, t *CloudTask) (resumeOptions, error) {
	action := req.Action
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
	case TaskActionRetryTransferOverwrite:
		if !t.DownloadDone {
			return resumeOptions{}, fmt.Errorf("下载尚未完成，无法只重做转存（请用 retry_download 续传）")
		}
		if t.Transfer == nil {
			return resumeOptions{}, fmt.Errorf("任务没有转存目标，无法重做转存")
		}
		return resumeOptions{keepDownloadDone: true}, nil
	case TaskActionRetryFromURL:
		if req.URL == "" {
			return resumeOptions{}, fmt.Errorf("retry_from_url 需要提供新的源 URL")
		}
		return resumeOptions{}, nil // 与 retry_download 同语义（保留断点续传）
	default:
		return resumeOptions{}, fmt.Errorf("未知任务动作 %q", action)
	}
}

// verifyActionPreconditions 做**执行前**的物理校验（只有服务端知道的事实）。
//
// retry_transfer 的前提是「产物仍在磁盘」：DownloadDone 只表示下载曾成功，产物可能已被
// disk_full 清理或人工删除。缺失时必须明确报错——否则 executeDownload 会静默回落整份下载，
// 与用户「只重做转存」的意图不符（且慢源代价高）。
func (m *CloudDownloadManager) verifyActionPreconditions(action TaskAction, t *CloudTask) error {
	if action != TaskActionRetryTransfer && action != TaskActionRetryTransferOverwrite {
		return nil
	}
	dir := m.TaskDirFor(t.Owner, t.ID)
	if dir == "" {
		return fmt.Errorf("租户不可用，无法校验下载产物")
	}
	dest := filepath.Join(dir, t.Filename)
	if st, err := os.Stat(dest); err != nil || st.IsDir() {
		return fmt.Errorf("下载产物已不存在（%s），无法只重做转存；请用 retry_download 重新下载", dest)
	}
	return nil
}

// TaskActionPlan 描述某任务**当前可用**的动作（供 v2 页面渲染操作菜单）。
type TaskActionPlan struct {
	Action   TaskAction `json:"action"`
	Label    string     `json:"label"`
	Hint     string     `json:"hint"`
	Disabled bool       `json:"disabled,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	// NeedsURL 表示执行该动作需要用户提供额外参数（目前仅 retry_from_url 的 url）。
	NeedsURL bool `json:"needs_url,omitempty"`
}

// PlanTaskActions 返回任务的可用动作清单（failed/cancelled 才有动作；其他状态返回空）。
//
// 为什么放在服务端：可用性判据（产物是否完整/是否有备选源）只有服务端知道，
// 前端不应重复推断——否则会渲染出必然失败（或语义被吞掉）的按钮。
func PlanTaskActions(t *CloudTask) []TaskActionPlan {
	if t == nil {
		return nil
	}
	// completed+damaged 走「定点修复优先」的续传动作（2026-11-26）：damaged 是"警告继续"
	// 放行后的完成态，用户需要一键修复入口。
	damaged := t.Status == "completed" && t.IntegrityStatus == "damaged"
	if !damaged && t.Status != "failed" && t.Status != "cancelled" {
		return nil
	}
	download := TaskActionPlan{Action: TaskActionRetryDownload, Label: "续传下载", Hint: "保留已下载部分，从断点继续（不重下已完成块）"}
	if t.Downloaded <= 0 {
		// 实测（2026-11-26）：取消/删除后断点已被清理时，提示必须诚实——否则用户以为能续传。
		download.Hint = "从零重新下载（无可续断点：上次结束时已清理残留）"
	}
	if damaged {
		download.Label = "修复重下"
		download.Hint = "先做零尾扫描 + 定点补片；修不好再整份重下"
	}
	plan := []TaskActionPlan{download}
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
	// 覆盖重试：仅当上次失败是目标冲突时可用（其余情况会白跑一次转存）。
	overwrite := TaskActionPlan{Action: TaskActionRetryTransferOverwrite, Label: "覆盖转存",
		Hint: "目标已存在时覆盖写入（显式 opt-in，仅影响本任务）"}
	if !transfer.Disabled && (t.Diagnostics == nil || t.Diagnostics.LastErrorClass != ErrClassTargetConflict) {
		overwrite.Disabled = true
		overwrite.Reason = "仅目标冲突时可用"
	}
	if transfer.Disabled {
		overwrite.Disabled = true
		overwrite.Reason = transfer.Reason
	}
	plan = append(plan, overwrite)

	// 换新链接重试：分享失效后由用户提供新 URL（保留断点续传）。
	plan = append(plan, TaskActionPlan{Action: TaskActionRetryFromURL, Label: "换链接重试",
		Hint: "提供新的源 URL 后重试（保留已下载断点）", NeedsURL: true})

	plan = append(plan, rotate)

	plan = append(plan, TaskActionPlan{
		Action: TaskActionDiscardPartial, Label: "清残留重下", Hint: "删除已有产物与断点，从零重新下载",
	})
	return plan
}

// mutateTaskForAction 在 resume 之前改写任务意图（覆盖开关 / 新源 URL）。
//
// 为什么放在 resume 之前：`resumeTaskWith` 会按当前 task 字段启动执行，
// 覆盖开关与 URL 必须先落地，否则本次重试仍走旧语义。
func (m *CloudDownloadManager) mutateTaskForAction(taskID string, req TaskActionRequest, owner string) error {
	switch req.Action {
	case TaskActionRetryTransferOverwrite:
		m.mu.Lock()
		t, ok := m.tasks[taskID]
		if !ok || !ownerVisible(t.Owner, owner) {
			m.mu.Unlock()
			return fmt.Errorf("task not found: %s", taskID)
		}
		if t.Transfer == nil {
			m.mu.Unlock()
			return fmt.Errorf("任务没有转存目标，无法覆盖转存")
		}
		t.Transfer.Overwrite = true // 显式 opt-in：只影响本任务
		t.UpdatedAt = time.Now()
		m.mu.Unlock()
		return nil
	case TaskActionRetryFromURL:
		m.mu.Lock()
		t, ok := m.tasks[taskID]
		if !ok || !ownerVisible(t.Owner, owner) {
			m.mu.Unlock()
			return fmt.Errorf("task not found: %s", taskID)
		}
		t.adoptURL(req.URL)
		t.UpdatedAt = time.Now()
		m.mu.Unlock()
		return nil
	default:
		return nil
	}
}
