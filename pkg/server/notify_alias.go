// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// notify_alias.go 是**迁移期兼容层**：通知域实现已下沉 pkg/server/notify，
// 本文件用类型别名让装配层存量代码（handlers 字段、alerts 引擎、审计 dispatch）
// 与存量测试零改动。
//
// **移除条件**：装配层拆出各切片、且 alerts 域搬入 pkg/server/alerts 后删除本文件；
// 新代码请直接使用 pkg/server/notify。
package server

import "github.com/cocomhub/sproxy/pkg/server/notify"

type (
	// NotifyCenter 是通知中心（注册表 + 规则路由 + 去抖 + 重试 + 有界历史）。
	NotifyCenter = notify.NotifyCenter
	// Notifier 是通知渠道接口。
	Notifier = notify.Notifier
	// NotifyMessage 是渠道统一载荷。
	NotifyMessage = notify.NotifyMessage
	// Event 是通知域事件（与 AuditEvent 解耦）。
	Event = notify.Event
)
