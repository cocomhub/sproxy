// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"time"
)

// 审计结果取值。
const (
	// AuditResultSuccess 表示操作成功完成。
	AuditResultSuccess = "success"
	// AuditResultDenied 表示操作被拒绝（校验失败/权限不足等客户端侧拒绝）。
	AuditResultDenied = "denied"
	// AuditResultError 表示操作因错误失败（IO 错误/对象不存在等）。
	AuditResultError = "error"
)

// AuditEvent 是一次敏感操作的结构化审计事件。
// 通过 RecordAudit 输出为固定 JSON 结构，可检索「谁在何时对哪个对象做了什么」。
type AuditEvent struct {
	Action     string    // 操作名：delete / rename / version_restore / version_delete / config_update / cloud_cancel / cloud_delete
	Actor      string    // 操作主体：AccessKey（SproxySig）或 APIKey 名；未认证为空串
	Mesh       string    // 所属 mesh（SproxySig 派生；APIKey/未认证为空串）
	ObjectType string    // 对象类型：file / task / config
	Object     string    // 对象标识：文件路径 / 任务 ID / config
	Result     string    // 结果：success / denied / error
	Detail     string    // 补充信息（如 rename 的目标、version_id、失败原因）
	TS         time.Time // 事件发生时间（零值自动填充当前时间）
}

// RecordAudit 输出一条结构化审计日志。
//
// 审计 logger 独立于业务 logger（Handlers.auditLogger）：固定 JSON 格式、不随
// log_format 配置切换，保证审计行可机器检索；actor/mesh 未显式填写时自动从请求
// ctx 读取（authMiddleware 注入）。RecordAudit 不 panic：auditLogger 为 nil 时
// 回退 slog.Default()，审计事件绝不因日志故障中断业务。
func (h *Handlers) RecordAudit(ctx context.Context, evt AuditEvent) {
	if evt.Actor == "" {
		evt.Actor = ActorFrom(ctx)
	}
	if evt.Mesh == "" {
		evt.Mesh = MeshFrom(ctx)
	}
	if evt.TS.IsZero() {
		evt.TS = time.Now()
	}
	// 挂钩有界内存环形缓冲：TS 填充后 Add 到 ring（ring 为 nil = 关闭，Add 静默跳过）。
	// 所有现有录入点（delete/rename/config_update/cloud_* 等）经本函数自动进 ring，
	// 无需逐点修改。Add 不 panic，审计链路绝不影响业务。
	h.addToAuditRing(evt)
	logger := h.auditLogger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "audit",
		"action", evt.Action,
		"actor", evt.Actor,
		"mesh", evt.Mesh,
		"object_type", evt.ObjectType,
		"object", evt.Object,
		"result", evt.Result,
		"detail", evt.Detail,
		"ts", evt.TS.Format(time.RFC3339Nano),
	)
}

// addToAuditRing 把已填好 TS 的最终审计事件写入有界环形缓冲（nil ring 静默跳过）。
// ring 容量满时环形覆盖丢弃最旧；本方法绝不 panic，审计链路不影响业务路径。
func (h *Handlers) addToAuditRing(evt AuditEvent) {
	if h.auditRing == nil {
		return
	}
	h.auditRing.Add(evt)
}
