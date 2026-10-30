// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package notify

import "time"

// Event 是通知域的**事件输入**：只包含路由/去抖/渲染需要的字段。
//
// 为什么不用 server.AuditEvent：通知是 server 作用域内的独立领域，不能反向依赖装配层
// （R6）；装配层在调用 Dispatch 前做一次字段映射（auditEventToNotify），
// 这样通知域可以独立测试与演进，审计字段变化也不会渗透进来。
type Event struct {
	Action string    // 操作名（delete / rename / config_update …）
	Object string    // 对象标识（文件路径 / 任务 ID / config）
	Result string    // 结果（success / denied / error）——参与去抖 key
	Detail string    // 补充信息
	TS     time.Time // 事件发生时间
}
