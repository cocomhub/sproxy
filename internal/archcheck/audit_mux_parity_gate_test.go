// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// audit_mux_parity_gate_test.go 是门禁 **R42**：**两个 mux 的审计中间件必须同源**。
//
// 背景（评审 R14 C13）：`auditMiddleware`（传输层兜底）与 `auditVolumeMiddleware`（卷维度绑定）
// 此前**只**包在主 mux（`routes.go` 的 `h.handler` 链）上；`routes_setup.go` 组装的隧道内层
// （`localHandler`，CLI/隧道用户的主路径）完全没有 ⇒ 经隧道的变更型请求既无兜底审计，也没有卷
// 归属（多卷部署下 Volume 恒空），而编译与既有测试都不会报。
//
// 本门禁把「两条链路用同一组中间件」变成机械约束：任一侧漏挂即红。新增审计类中间件时，
// 请同时加入本表的 `wantMiddlewares`。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantMiddlewares 是必须同时出现在两条链路上的审计中间件。
var wantMiddlewares = []string{"auditMiddleware(", "auditVolumeMiddleware("}

// auditChainFiles 是两条链路的装配文件（主 mux / 隧道内层）。
var auditChainFiles = []string{
	"pkg/server/routes.go",
	"pkg/server/routes_setup.go",
}

// TestR42_AuditMiddlewareOnBothMuxes 断言两条链路都挂了同一组审计中间件。
func TestR42_AuditMiddlewareOnBothMuxes(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	for _, rel := range auditChainFiles {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读 %s: %v", rel, err)
		}
		src := string(b)
		for _, mw := range wantMiddlewares {
			if !strings.Contains(src, mw) {
				t.Fatalf("R42 违规：%s 未挂 %s —— 审计中间件必须在**两个 mux** 上同源"+
					"（否则隧道/CLI 路径的变更型请求无兜底审计、卷维度恒空）", rel, mw)
			}
		}
	}
}
