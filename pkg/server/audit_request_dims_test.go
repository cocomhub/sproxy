// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_request_dims_test.go 锁定评审 R13 P2：审计条目的**请求维度**（`request_id` / `source_ip`）
// 必须真正接线（此前生产 `CtxFields` 只有 Actor/Owner/Mesh ⇒ 这两个字段恒为空，
// 而设计 §5.7 把它们列为读面敏感字段、覆盖率矩阵却标 ✅）。

import (
	"context"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditEntry_RequestDimsFilled 真实请求产生的审计条目必须带 request_id 与 source_ip。
func TestAuditEntry_RequestDimsFilled(t *testing.T) {
	t.Parallel()
	url, h := newAuditVolumeServer(t) // admin 回环 + 真实审计装配（含 CtxFields）

	body := []byte("dims-probe")
	if st, resp := uploadFile(t, url, "dims.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "upload"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("上传应产生 upload 审计行")
	}
	ev := entries[len(entries)-1]
	if ev.RequestID == "" {
		t.Error("request_id 应为该请求的 trace id（CtxFields.RequestID 未接线？）")
	}
	if ev.SourceIP == "" {
		t.Error("source_ip 应来自请求 RemoteAddr（CtxFields.SourceIP 未接线？）")
	}
}
