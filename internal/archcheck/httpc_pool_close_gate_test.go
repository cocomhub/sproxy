// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"strings"
	"testing"
)

// TestHTTPCIsolatedWhenClosingIdleConns —— 门禁：**调用 `CloseIdleConnections()` 的 httpc 客户端
// 必须是隔离池**（显式 `Isolated: true` 或自备 `Transport`）。
//
// 背景（2026-11-08 真实回归）：`pkg/httpc` 的默认 Transport 由"独立池"改为"**进程级共享池**"
// （对齐 netutil 的既定设计）后，若干**原本依赖隔离**且会 `defer CloseIdleConnections()` 的调用点
// （client SDK / tunnel / cmd）自动退化为"关闭进程共享池" —— 会**打断全进程在途请求**
// （硬规则 17 的原始事故形态），且这些点的注释仍写着"隔离"，**注释与行为矛盾**。
//
// 判据（文件级近似，够用且零误报）：
//   - 文件含 `CloseIdleConnections(`；
//   - 且该文件含 httpc 客户端构造（`httpc.New` / `httpc.NewHTTPClient`）；
//   - ⇒ 文件内必须出现 `Isolated: true` 或 `Transport:`（显式指定池）。
//
// 局限（已刻意接受）：同文件多个 client 时是"文件级"判定，不区分具体实例；若要更严需 AST 分析，
// 收益不足。**直接操作自有 transport 的关闭**（如 `tr.CloseIdleConnections()` 且文件无 httpc 构造）
// 不在本门禁范围（它们不涉及共享池选择）。
//
// 登记基线：新增调用点必须显式指定池 ⇒ 无需基线文件；变异验证见提交说明。
func TestHTTPCIsolatedWhenClosingIdleConns(t *testing.T) {
	t.Parallel()
	var violations []string
	for _, rel := range walkSourceFiles(t, func(rel string, data []byte) string {
		if strings.HasSuffix(rel, "_test.go") {
			return "" // 测试自建 client 由 testutil.IsolatedClient 保证
		}
		src := string(data)
		if !strings.Contains(src, "CloseIdleConnections(") {
			return ""
		}
		if !strings.Contains(src, "httpc.New") && !strings.Contains(src, "httpc.NewHTTPClient") {
			return "" // 不涉及 httpc 的池选择（自有 transport）
		}
		if strings.Contains(src, "Isolated: true") || strings.Contains(src, "Transport:") {
			return ""
		}
		return "含 CloseIdleConnections() 但未显式指定隔离池（Isolated: true）或自备 Transport"
	}) {
		violations = append(violations, "  "+rel)
	}
	if len(violations) > 0 {
		t.Fatalf("关闭空闲连接必须针对**自有连接池**（共享池的生命周期属于进程，关闭会打断"+
			"全进程在途请求——硬规则 17）：\n%s\n修法：httpc.New(httpc.Options{Isolated: true, ...})"+
			"（短命 client）或自备 Transport（每实例/每 peer 独立克隆）。",
			strings.Join(violations, "\n"))
	}
}
