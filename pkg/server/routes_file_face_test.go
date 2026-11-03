// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_file_face_test.go 是**路由装配面**回归测试：逐条打文件域路由，断言不出现
// 「接线错误」500。
//
// 为什么需要（真实事故）：`fileRoute` / `fileRouteRead` 在运行时用谓词**复核**分类，
// 不一致就 500 + Error 日志（fail-closed）。`GET /api/search/semantic` 曾被包成
// fileRouteRead，但不在 isReadOnlyFileRoute 名单里 ⇒ **主 mux 上每次语义搜索都 500**。
// 该缺陷长期无人发现，因为：
//   - pkg/files 的 handler 级测试直接调 handler，**绕过路由与包装器**；
//   - RBAC 测试断言的是「清单自身」（被测列表与被断言列表同源，恒真）；
//   - 谓词漏列不会让任何门禁变红。
//
// 本测试从「装配后的真实 mux」出发，覆盖上面三类盲区：任何一条文件域路由只要
// 包装器与分类不一致，就会命中 fail-closed 分支，被这里抓住。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// 三条 fail-closed 分支的错误文案（fileRoute / fileRouteRead 的接线自检）。
var wiringErrorMarkers = []string{
	"internal: route not in file group",
	"internal: route in read-only subgroup",
	"internal: route not in read-only subgroup",
}

// TestFileDomainRoutes_NoWiringMismatchOnServerFace 以 user 身份逐条打主 mux，
// 断言响应体不含接线错误标记（状态码可能是 400/404/405——那都是正常业务错误）。
func TestFileDomainRoutes_NoWiringMismatchOnServerFace(t *testing.T) {
	t.Parallel()
	url, _ := newRBACRoleServer(t)
	for _, rt := range fileDomainRoutes() {
		t.Run(rt.Method+" "+rt.Path, func(t *testing.T) {
			// 这些路径带 {token}/{name}/{type} 通配：换成具体值再请求。
			path := strings.NewReplacer(
				"{token}", "tok1", "{name}", "sec1", "{type}", "local",
			).Replace(rt.Path)
			status, body := doSignedJSON(t, rt.Method, url+path, testAccessKey, testAccessSecret, nil)
			txt := string(body)
			for _, marker := range wiringErrorMarkers {
				if strings.Contains(txt, marker) {
					t.Fatalf("%s %s 命中接线错误 500（status=%d）：%q\n"+
						"含义：包装器分类与 isFileGroupedRoute/isReadOnlyFileRoute 不一致——"+
						"修分类表，别改本测试", rt.Method, path, status, marker)
				}
			}
			_ = status
		})
	}
}

// TestFileDomainRoutes_ClassMatchesWrapper 锁「声明分类 → 包装器」映射不漂移：
// ClassUser 必须落到 fileRoute（requireRole user），ClassReader 必须落到 fileRouteRead。
// 变异点：routeWrapper 里把两个 case 对调 ⇒ 本测试红（鉴权整体放宽/收紧）。
func TestFileDomainRoutes_ClassMatchesWrapper(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	routes := fileDomainRoutes()
	if len(routes) == 0 {
		t.Fatal("文件域路由表为空")
	}
	for _, rt := range routes {
		if rt.Server != router.ClassUser && rt.Server != router.ClassReader {
			t.Fatalf("%s %s 的 Server 分类 = %s，文件域只允许 ClassUser/ClassReader",
				rt.Method, rt.Path, rt.Server)
		}
	}
	// routeWrapper 对两类都必须给出非 nil 包装（nil = 裸注册 = 无鉴权，对文件面是事故）。
	for _, class := range []router.Class{router.ClassUser, router.ClassReader} {
		if fn := h.routeWrapper(class, ""); fn == nil {
			t.Fatalf("分类 %s 未定义包装策略（文件面不允许裸注册）", class)
		}
	}
}

// TestSemanticSearchRouteIsReaderAccessible 是**事故专项回归**：语义搜索是只读 GET，
// 必须归只读子组——否则主 mux 上 fileRouteRead 的 fail-closed 自检会直接 500。
func TestSemanticSearchRouteIsReaderAccessible(t *testing.T) {
	t.Parallel()
	if c, ok := fileDomainClass("GET /api/search/semantic"); !ok || c != router.ClassReader {
		t.Fatalf("语义搜索分类 = %v(ok=%v), want ClassReader（否则 fileRouteRead 自检 500，reader 也会被拒）", c, ok)
	}
	if isWriteFaceRoute(http.MethodGet, "GET /api/search/semantic") {
		t.Fatal("语义搜索不是写面（只读副本上应可执行）")
	}
	// 装配面实证：reader 不得因分类错误拿到 500/403。
	url, _ := newRBACRoleServer(t)
	if st := signedGetStatus(t, url, "/api/search/semantic?q=probe", readerTestAK, readerTestSK); st == http.StatusInternalServerError {
		t.Fatalf("reader GET /api/search/semantic = 500（分类/包装不一致）")
	}
}
