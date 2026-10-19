// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_secret_scenarios_test.go 补设计 §9.1 的**密钥类**审计场景（P1 缺口，风险最高）：
//
//  ① `secret_create` / `secret_export` / `secret_delete` 都必须有审计行，actor = 操作者；
//  ② **红线**：审计条目**绝不包含密钥值**（detail 只允许 mode/状态码）——审计读面在非管理员
//     维度可能扩展（§5.7），一旦值进了条目就等于值进了日志；
//  ③ **失败也留痕**：导出非法名（路径穿越 ⇒ 400）是攻击信号，必须记录（本文件同批修复）。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_Secrets 密钥全生命周期 + 失败路径的审计断言（含明文红线）。
func TestAuditScenario_Secrets(t *testing.T) {
	t.Parallel()
	h, _ := newSecretsHandlers(t, nil)
	const owner = "alice"
	ctx := context.Background()

	// ① 创建（服务端随机生成 ⇒ 值只出现在响应里）
	st, body := callSecretsHandler(h, owner, http.MethodPost, "/api/secrets", `{"name":"audit-k1","mode":"random"}`)
	if st != http.StatusOK {
		t.Fatalf("create status=%d body=%s", st, body)
	}
	var created struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("解析 create 响应: %v (body=%s)", err, body)
	}
	if created.Value == "" {
		t.Fatalf("random 创建应返回值, body=%s", body)
	}

	// ② 导出（明文返回；审计只记 action，不记值）
	if st, body := callSecretsHandler(h, owner, http.MethodGet, "/api/secrets/audit-k1", ""); st != http.StatusOK {
		t.Fatalf("export status=%d body=%s", st, body)
	}

	// ③ 失败导出（路径穿越 ⇒ 400）——攻击信号必须留痕
	if st, _ := callSecretsHandler(h, owner, http.MethodGet, "/api/secrets/..", ""); st != http.StatusBadRequest {
		t.Fatalf("非法名导出应 400, got %d", st)
	}

	// ④ 删除
	if st, body := callSecretsHandler(h, owner, http.MethodDelete, "/api/secrets/audit-k1", ""); st != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", st, body)
	}

	entries := h.readAuditEvents(ctx, audit.EntryFilter{}, 0, false)
	if len(entries) == 0 {
		t.Fatal("密钥操作应产生审计行（埋点缺失或读面不通 ✗）")
	}

	// 断言 1：三类动作都有行，且 actor 正确。
	want := map[string]bool{"secret_create": false, "secret_export": false, "secret_delete": false}
	for _, ev := range entries {
		if _, ok := want[ev.Action]; !ok {
			continue
		}
		want[ev.Action] = true
		if ev.Actor != owner {
			t.Errorf("%s actor = %q, want %q", ev.Action, ev.Actor, owner)
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("缺少 %s 审计行", action)
		}
	}

	// 断言 2（红线）：任何条目的 detail 都不能包含密钥值或其前缀。
	for _, ev := range entries {
		if strings.Contains(ev.Detail, created.Value) {
			t.Fatalf("审计条目泄露密钥值 ✗ action=%s detail=%q", ev.Action, ev.Detail)
		}
		if len(created.Value) >= 16 && strings.Contains(ev.Detail, created.Value[:16]) {
			t.Fatalf("审计条目泄露密钥值前缀 ✗ action=%s detail=%q", ev.Action, ev.Detail)
		}
	}

	// 断言 3：失败的导出也要留痕（result != success）。
	failed := false
	for _, ev := range entries {
		if ev.Action == "secret_export" && ev.Object == ".." {
			failed = true
			if ev.Result == "success" {
				t.Errorf("失败导出不能记 success, got %q", ev.Result)
			}
		}
	}
	if !failed {
		t.Error("非法名导出（攻击信号）必须有审计行 ✗")
	}
}
