// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import "testing"

// TestRecordAuditRotate_NilReceiverNoPanic 回归：nil 接收者必须 no-op。
// 缺陷：原实现 `if m == nil || reason == ""` 只兜住 reason 分支，随后仍访问
// m.auditRotate.add ⇒ nil 指针 panic（与注释「无 metrics 时 no-op」矛盾）。
func TestRecordAuditRotate_NilReceiverNoPanic(t *testing.T) {
	t.Parallel()
	var m *Metrics
	m.RecordAuditRotate("size")
	m.RecordAuditRotate("")
}

// TestRecordAuditRotate_EmptyReasonBucketed 空 reason 归入 unknown（非 nil 路径零回归）。
func TestRecordAuditRotate_EmptyReasonBucketed(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	m.RecordAuditRotate("")
	samples := m.AuditRotateSamples()
	if len(samples) != 1 || samples[0].Labels != `reason="unknown"` || samples[0].Value != 1 {
		t.Fatalf("samples=%+v，want reason=unknown value=1", samples)
	}
}
