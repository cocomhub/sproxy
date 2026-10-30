// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// alerts_alias.go 是告警引擎的**迁移期别名**（实现已下沉 pkg/server/alerts）。
// 删除条件：装配层与 cmd/* 的 AlertEngine 引用全部改为 alerts.Engine。
package server

import "github.com/cocomhub/sproxy/pkg/server/alerts"

// AlertEngine 是 alerts.Engine 的兼容别名。
type AlertEngine = alerts.Engine

// SourceNATFailure 是 NAT 失败告警源（与 alerts 包同一常量）。
const SourceNATFailure = alerts.SourceNATFailure
