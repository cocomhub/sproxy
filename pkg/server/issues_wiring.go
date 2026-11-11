// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// IssueCenter 是**待处理事项**在装配层的中枢：持有唯一的 `issues.Manager`，并把它的能力
// 适配成各领域包需要的最小接口（如 `secretdata.IssueReporter`）。
//
// 为什么要这一层（用户裁定 2026-11-10）：
//   - 领域包（`pkg/volume/...`）**不得**反向依赖 `pkg/server`，因此它们只声明**最小接口**
//     （`IssueReporter`）；本层负责把 `issues.Manager` 适配进去；
//   - 装配根只创建**一个** IssueCenter（唯一 Manager 实例）⇒ 所有组件上报汇聚到同一份
//     持久化账本，Web/API 才能看到**全量**未成功事项。
//
// 设计约束：路径经参数注入（测试用 t.TempDir），不读全局状态。
type IssueCenter struct {
	manager *issues.Manager
}

// NewIssueCenter 在 `storePath` 建立事项账本（空路径 ⇒ 退化为内存存储，仅用于测试/禁用持久化）。
func NewIssueCenter(storePath string) *IssueCenter {
	var st issues.Store
	if storePath == "" {
		st = issues.NewMemStore()
	} else {
		st = issues.NewFileStore(storePath)
	}
	return &IssueCenter{manager: issues.New(issues.Options{
		Store: st,
		// 定期重试的退避：首次 5 分钟，指数增长，上限 1 小时（与"最终必须成功"配套：
		// 有界重试 + 达上限转人工，绝不无限重试也绝不静默放弃）。
		RetryBackoff: 5 * time.Minute,
		MaxAttempts:  5,
		MaxBackoff:   time.Hour,
	})}
}

// Manager 暴露底层 Manager（供 API/Web UI 与调度器使用）。
func (c *IssueCenter) Manager() *issues.Manager {
	if c == nil {
		return nil
	}
	return c.manager
}

// ReportStorageIssue 实现领域包的最小上报接口（如 `secretdata.IssueReporter`）。
//
// 约定：`kind` 是领域包定义的稳定类型码，必须与 `issues.Kind` 取值对齐（见 issues.Kind* 常量）；
// `detail` 必须写明「发生了什么 + 为何不能自动决策 + 人工需要做什么」。
func (c *IssueCenter) ReportStorageIssue(volume, kind, path, detail string, retryable bool) {
	if c == nil || c.manager == nil {
		return
	}
	c.manager.Report(issues.Item{
		Kind:      issues.Kind(kind),
		Volume:    volume,
		Path:      path,
		Detail:    detail,
		Source:    "storage",
		Retryable: retryable,
	})
}

// DefaultIssuesStorePath 返回事项账本的默认落盘路径（与其它 state 同级，便于运维查看/备份）。
//
// 放在 `meta/state/` 下与既有状态文件（索引快照、凭证快照等）同目录：运维只需看一个地方。
func DefaultIssuesStorePath(storageRoot string) string {
	if storageRoot == "" {
		return ""
	}
	return filepath.Join(storageRoot, "meta", "state", "issues.json")
}

// RunIssuesOnce 执行一轮到期重试（供调度器周期调用）。
//
// executors：Kind → 执行器；**未注册执行器的 Kind 不会被静默跳过**，而是转 `needs_human`
// （由 Manager 实现，避免"以为是自动重试、其实永远没跑"）。
func (c *IssueCenter) RunIssuesOnce(ctx context.Context, now time.Time, executors map[issues.Kind]issues.RetryFunc) issues.RetryResult {
	if c == nil || c.manager == nil {
		return issues.RetryResult{}
	}
	return c.manager.RunDue(ctx, now, executors)
}
