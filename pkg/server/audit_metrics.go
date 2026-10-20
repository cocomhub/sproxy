// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_metrics.go 把 `pkg/audit` 的观测接口接到本服务的 `/metrics`（评审 R13 P1）。
//
// 背景：此前生产装配从未注入 `audit.Metrics` ⇒ 恒 `NullMetrics` ⇒
// `audit_dropped`（**真正丢失**的条目）/ `write_failed` / `hook_panics` / `rotate` 全部不可观测，
// 而文档/设计把 `audit_dropped` 当作可观测契约 —— 「有界丢弃」成了静默失效。

import "github.com/cocomhub/sproxy/pkg/audit"

// auditMetrics 是 `audit.Metrics` 到 `server.Metrics` 的适配器（nil 安全）。
type auditMetrics struct{ m *Metrics }

// newAuditMetrics 构造适配器（m 为 nil 时为 no-op）。
func newAuditMetrics(m *Metrics) auditMetrics { return auditMetrics{m: m} }

// IncrDrop 累计**未持久化即被逐出**的条目数（真正的审计丢失）。
func (a auditMetrics) IncrDrop(n int) {
	if a.m != nil {
		a.m.AuditDropped.Add(int64(n))
	}
}

// IncrEvict 累计已持久化条目的热索引逐出数（不是丢失）。
func (a auditMetrics) IncrEvict(n int) {
	if a.m != nil {
		a.m.AuditEvicted.Add(int64(n))
	}
}

// IncrWriteFailed 累计落盘失败次数。
func (a auditMetrics) IncrWriteFailed() {
	if a.m != nil {
		a.m.AuditWriteFailed.Add(1)
	}
}

// IncrHookPanic 累计下游钩子 panic 次数。
func (a auditMetrics) IncrHookPanic() {
	if a.m != nil {
		a.m.AuditHookPanics.Add(1)
	}
}

// IncrRotate 累计轮转次数（按原因分桶）。
func (a auditMetrics) IncrRotate(reason string) {
	if a.m != nil {
		a.m.RecordAuditRotate(reason)
	}
}

// 编译期断言：适配器满足 `audit.Metrics`。
var _ audit.Metrics = auditMetrics{}
