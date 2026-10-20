// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_testutil_test.go 提供审计场景测试的**唯一**接线助手：把真实 `pkg/audit` 服务
// （journal + ring index，与生产 `setupAudit` 同构）挂到 `*Handlers` 上，使断言可以走
// `/api/audit` 的同一读入口（`readAuditEvents`），而不是测试专用旁路。
//
// 收敛点：`volume` / `secret` / `presign` 等场景用例共用本助手（此前每个文件各写一份 ✗）。

import (
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// attachTestAudit 给 h 挂上真实审计服务（journal 在 t.TempDir()），返回该 journal。
func attachTestAudit(t *testing.T, h *Handlers) *audit.FileJournal {
	t.Helper()
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir:        filepath.Join(t.TempDir(), "audit-hot"),
		Layout:     audit.LayoutGlobal,
		GlobalName: "audit.log",
		SyncMode:   audit.SyncNone,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	caps := audit.FullCapabilities()
	svc, serr := audit.New(audit.Options{
		Journal: j,
		Index:   audit.NewRingIndex(256),
		Logger:  testLogger(),
		Policy:  audit.Policy{MaxMemoryEntries: 256},
		Caps:    &caps,
		// 与生产 setupAudit 同构（**测试助手必须镜像生产接线**，否则维度类用例恒绿）：
		// 卷/请求维度也注入，`Entry.Volume` 等字段才会被真实填充路径覆盖。
		CtxFields: audit.ContextFields{
			Actor:     ActorFrom,
			Owner:     ActorFrom,
			Mesh:      MeshFrom,
			RequestID: auditRequestIDFrom,
			SourceIP:  auditSourceIPFrom,
			Volume:    auditVolumeFrom,
		},
	})
	if serr != nil {
		t.Fatalf("audit.New: %v", serr)
	}
	h.auditSvc = svc
	h.auditJournal = j
	return j
}
