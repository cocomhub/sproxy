// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// systemAuditScope 是请求面合规事件的**合成作用域**（设计 E1：所有审计都有作用域）。
const systemAuditScope = "system"

// 审计结果取值（对外视图契约）。
const (
	// AuditResultSuccess 表示操作成功完成。
	AuditResultSuccess = "success"
	// AuditResultDenied 表示操作被拒绝（校验失败/权限不足等客户端侧拒绝）。
	AuditResultDenied = "denied"
	// AuditResultError 表示操作因错误失败（IO 错误/对象不存在等）。
	AuditResultError = "error"
)

// AuditEvent 是一次敏感操作的结构化审计事件（**对外 JSON 视图**）。
//
// 字段带显式 json tag：/api/audit 直接序列化给 Web UI，前端 app-render.auditTableHtml 按
// 小写字段读取（action/actor/mesh/object_type/object/result/detail/ts）——务必与前端契约
// 一一对应（无 tag 会按大写字段名序列化，导致审计表整行显示占位符 "-"）。
//
// 与 pkg/audit.Entry 的关系：Entry 是**统一条目模型**（唯一实现），本结构是它的**读面投影**
// （auditEventFromEntry 是唯一转换点）。写入侧不再有第二套实现。
type AuditEvent struct {
	Action     string    `json:"action"`      // 操作名：delete / rename / version_restore / config_update / cloud_cancel …
	Actor      string    `json:"actor"`       // 操作主体：AccessKey（SproxySig）或 APIKey 名；未认证为空串
	Mesh       string    `json:"mesh"`        // 所属 mesh（SproxySig 派生；APIKey/未认证为空串）
	ObjectType string    `json:"object_type"` // 对象类型：file / task / config
	Object     string    `json:"object"`      // 对象标识：文件路径 / 任务 ID / config
	Result     string    `json:"result"`      // 结果：success / denied / error
	Detail     string    `json:"detail"`      // 补充信息（如 rename 的目标、version_id、失败原因）
	TS         time.Time `json:"ts"`          // 事件发生时间
}

// auditEventFromEntry 把统一审计条目投影为对外审计事件（**唯一转换点**）。
func auditEventFromEntry(e audit.Entry) AuditEvent {
	return AuditEvent{
		Action:     e.Action,
		Actor:      e.Actor,
		Mesh:       e.Mesh,
		ObjectType: e.ObjType,
		Object:     e.Object,
		Result:     string(e.Result),
		Detail:     e.Detail,
		TS:         e.TS,
	}
}

// auditEventsFromEntries 批量投影（nil 入参 ⇒ 空切片，保持 JSON `[]` 而非 null）。
func auditEventsFromEntries(es []audit.Entry) []AuditEvent {
	out := make([]AuditEvent, 0, len(es))
	for _, e := range es {
		out = append(out, auditEventFromEntry(e))
	}
	return out
}

// RecordAudit 输出一条结构化审计日志（经**唯一审计服务** pkg/audit 写入）。
//
// actor/mesh 未显式填写时自动从请求 ctx 读取（authMiddleware 注入）；Owner 取 **AK 租户键**
// （设计 §5.7：权限过滤的唯一权威字段 = Owner，且**不得**用 Principal.Owner——它是 IdP claim）。
// 通知中心 dispatch 与审计 slog 行保持原语义。RecordAudit 不 panic：审计失败绝不中断业务。
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
	if h.auditSvc != nil {
		_ = h.auditSvc.Emit(ctx, audit.Entry{
			Scope:   audit.ScopeID(systemAuditScope),
			Kind:    audit.KindEvent,
			Type:    audit.TypeSensitive,
			Action:  evt.Action,
			Actor:   evt.Actor,
			Owner:   ActorFrom(ctx), // 租户键 = AK（非 Principal.Owner）
			Mesh:    evt.Mesh,
			ObjType: evt.ObjectType,
			Object:  evt.Object,
			Result:  audit.Result(evt.Result),
			Detail:  evt.Detail,
			TS:      evt.TS,
		})
	}
	// 通知中心 dispatch：按规则路由到渠道（异步，绝不阻塞审计/业务）；nil = 未启用零回归。
	if h.notifyCenter != nil {
		h.notifyCenter.Dispatch(ctx, evt)
	}
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
