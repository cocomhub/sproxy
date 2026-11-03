// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// routes_files_equiv_test.go 是**差分等价测试**：文件域路由迁入 `routes_files.go` 后，
// 新的「声明式分类」必须与旧的三个字符串谓词逐条一致。
//
// 为什么需要：迁移的收益是删掉 `isFileGroupedRoute` / `isReadOnlyFileRoute` /
// `isWriteFaceRoute` 三份白名单，但删除前必须证明新表与原行为**等价**——否则删掉的是
// 行为而不是重复。本测试在谓词仍存在期间逐条断言；谓词删除后本测试同步删除（见设计
// docs/designs/2026-11-03-route-class-single-source.md 批次 B4）。
//
// 失败含义：新表的分类与线上实际生效的策略不一致 ⇒ 迁移会改变鉴权/审计行为。

import (
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// TestFileDomainRoutes_ClassMatchesLegacyPredicates 逐条断言声明式分类与旧谓词一致。
func TestFileDomainRoutes_ClassMatchesLegacyPredicates(t *testing.T) {
	t.Parallel()
	routes := fileDomainRoutes()
	if len(routes) == 0 {
		t.Fatal("文件域路由表为空（解析或表构造失效）")
	}
	for _, r := range routes {
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			t.Parallel()
			inGroup := isFileGroupedRoute(r.Path)
			readOnly := isReadOnlyFileRoute(r.Path, r.Method)

			// 1) 两条路由都必须在文件组内（否则 fileRoute 会 500 / localMuxGate 会放行）。
			if !inGroup {
				t.Fatalf("旧谓词判定 %s %s 不在文件组，但表里声明了文件域分类", r.Method, r.Path)
			}
			// 2) Server 分类必须与「只读子组」判定一致。
			wantServer := router.ClassUser
			if readOnly {
				wantServer = router.ClassReader
			}
			if r.Server != wantServer {
				t.Fatalf("%s %s Server 分类 = %s, want %s（旧谓词 readOnly=%v）",
					r.Method, r.Path, r.Server, wantServer, readOnly)
			}
			// 3) 写面判定必须与「非只读子组」一致。
			if got, want := isWriteFaceRoute(r.Path, r.Method), r.Server == router.ClassUser; got != want {
				t.Fatalf("%s %s isWriteFaceRoute = %v, want %v（Server=%s）",
					r.Method, r.Path, got, want, r.Server)
			}
			// 4) 隧道内层分类：文件域全部为 tunnel（裸注册，角色门由 localMuxGate 收口）。
			if r.Local != router.ClassTunnel {
				t.Fatalf("%s %s Local 分类 = %s, want ClassTunnel", r.Method, r.Path, r.Local)
			}
		})
	}
}

// TestFileDomainRoutes_NoDuplicatePattern 锁「同 (method, path) 不得声明两次」——
// 重复声明会让后一次静默覆盖前一次（ServeMux 也会 panic，但报错点离表很远）。
func TestFileDomainRoutes_NoDuplicatePattern(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, len(fileDomainRoutes()))
	for _, r := range fileDomainRoutes() {
		key := r.Method + " " + r.Path
		if _, ok := seen[key]; ok {
			t.Fatalf("文件域路由重复声明：%s", key)
		}
		seen[key] = struct{}{}
	}
}

// TestFileDomainRoutes_RegisteredOnBothFaces 锁「每个文件域路由确实注册到了两个面」，
// 防止表项写错路径（表里加了但 mux 上不生效 = 静默丢路由）。
func TestFileDomainRoutes_RegisteredOnBothFaces(t *testing.T) {
	t.Parallel()
	srv, local := http.NewServeMux(), http.NewServeMux()
	registered := router.Register(srv, local, filesRoutePrefix, fileDomainRoutes(),
		func(fn func(*Handlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) { fn(nil, w, r) }
		}, nil)
	if want := 2 * len(fileDomainRoutes()); registered != want {
		t.Fatalf("注册条数 = %d, want %d（两个面各一条）", registered, want)
	}
}
