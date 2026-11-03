// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_credentials_test.go 锁「公开凭据入口必须带入口控制（Guard）」。
//
// 为什么需要：分类（ClassPublic）只说明「不要求认证」，**不说明有暴力破解防护**。
// 三个公开入口各自挂独立限流器（+ 主 mux 的 IP 门）；Guard 字段一旦被删，分类仍
// 是 public、R38 仍绿、所有功能测试仍通过——只有这条断言会发现「公开端点裸奔」。
// 实测变异：删掉 login 的 Guard，R38 与全部凭据测试均为绿。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// TestCredentialRoutes_PublicEntriesCarryGuards 断言三个公开入口的 Guard 与预期一致，
// 且不存在「public 但无 Guard」的凭据路由（新增公开入口必须先定义入口控制）。
func TestCredentialRoutes_PublicEntriesCarryGuards(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"POST /api/credentials/register": guardCredentialRegister,
		"POST /api/credentials/nonce":    guardCredentialNonce,
		"POST /api/credentials/login":    guardCredentialLogin,
	}
	seen := map[string]bool{}
	for _, rt := range credentialRoutes() {
		key := rt.Method + " " + credentialRoutePrefix + rt.Path
		if rt.Server != router.ClassPublic {
			continue
		}
		seen[key] = true
		expected, ok := want[key]
		if !ok {
			t.Fatalf("%s 声明为 public 但未登记预期 Guard（新增公开入口必须显式声明入口控制，否则等于裸奔）", key)
		}
		if rt.Guard != expected {
			t.Fatalf("%s Guard = %q, want %q（丢失限流/IP 门 = 公开端点无暴力破解防护）", key, rt.Guard, expected)
		}
		// 双端都必须是 tunnel 内层（浏览器隧道模式登录页需可达）。
		if rt.Local != router.ClassTunnel {
			t.Fatalf("%s Local 分类 = %s, want ClassTunnel", key, rt.Local)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Fatalf("预期中的公开入口 %s 不存在（被删或改了分类）", key)
		}
	}
}
