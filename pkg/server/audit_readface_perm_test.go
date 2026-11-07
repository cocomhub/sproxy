// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_readface_perm_test.go 锁定设计 §5.7 / §9.1「读面权限矩阵」中**尚未被 e2e 覆盖**的部分：
//
//  ① **主体自身角色优先**（M1-11 修复点）：`Principal.Role == "admin"` 但**凭据环里没有该 AK**
//     的主体必须放行 —— 否则「无 AK 的管理员主体」（装配层注入 / 回环无认证兜底 / 服务端自同步）
//     会被误判为非管理员 ✗；
//  ② 非管理员（含未认证）恒拒绝，且**查询参数不能放宽**（传 `dim=system`/`owner=X` 仍拒绝）；
//  ③ `/api/audit` 与 `/api/audit/export` **同一裁决**（不能一个放宽一个收紧）。
//
// 设计 §5.7 的「用户维度视图 + 脱敏」尚未实现（`auditReadAllowed` 注释已写明是将来选项）⇒
// 本测试锁的是**当前 fail-closed 契约**：非管理员读不到任何全量视图。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// permCtxPrincipal 构造带 Principal 的请求 ctx（认证面的输出）。
func permCtxPrincipal(p *Principal) context.Context {
	return context.WithValue(context.Background(), principalCtxKey{}, p)
}

// TestAuditReadAllowed_Matrix 读面权限矩阵（fail-closed + 主体角色优先 + 参数不放宽）。
func TestAuditReadAllowed_Matrix(t *testing.T) {
	t.Parallel()
	h := &Handlers{} // 无凭据环 ⇒ getRole 恒 "user"（即「AK 不在环里」的最坏情况）

	cases := []struct {
		name    string
		ctx     context.Context
		query   string
		allowed bool
	}{
		{
			name:    "admin 主体（无 AK、凭据环无条目）⇒ 放行（M1-11 回归）",
			ctx:     permCtxPrincipal(&Principal{AK: "", Role: "admin"}),
			allowed: true,
		},
		{
			name:    "admin 主体（AK 不在环里）⇒ 放行",
			ctx:     permCtxPrincipal(&Principal{AK: "no-such-ak", Role: "admin"}),
			allowed: true,
		},
		{
			name:    "user 主体 ⇒ 拒绝",
			ctx:     permCtxPrincipal(&Principal{AK: "user-ak", Role: "user"}),
			allowed: false,
		},
		{
			name:    "user 主体 + dim=system ⇒ 仍拒绝（参数不放宽）",
			ctx:     permCtxPrincipal(&Principal{AK: "user-ak", Role: "user"}),
			query:   "?dim=system&owner=alice&actor=bob",
			allowed: false,
		},
		{
			name:    "无 Principal / 无 actor（未认证）⇒ 拒绝",
			ctx:     context.Background(),
			allowed: false,
		},
		{
			name:    "无 Principal + dim=system ⇒ 仍拒绝",
			ctx:     context.Background(),
			query:   "?dim=system",
			allowed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/api/audit"+tc.query, nil).WithContext(tc.ctx)
			if got := h.auditReadAllowed(req); got != tc.allowed {
				t.Fatalf("auditReadAllowed = %v, want %v", got, tc.allowed)
			}
			// 导出端点必须是**同一裁决**（共用一个函数 ⇒ 不能出现读面/导出面不一致 ✗）。
			ereq := httptest.NewRequest(http.MethodGet, "/api/audit/export"+tc.query, nil).WithContext(tc.ctx)
			if got := h.auditReadAllowed(ereq); got != tc.allowed {
				t.Fatalf("export 面裁决不一致: %v, want %v", got, tc.allowed)
			}
		})
	}
}

// TestAuditHandlers_NonAdminForbidden 两个端点对非管理员都返回 403（handler 层闭环）。
func TestAuditHandlers_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	ctx := permCtxPrincipal(&Principal{AK: "user-ak", Role: "user"})

	for _, tc := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"read", h.auditHandler, "/api/audit"},
		{"export", h.auditExportHandler, "/api/audit/export"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			tc.fn(rec, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403; body=%s", tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}
