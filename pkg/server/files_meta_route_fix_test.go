// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"testing"
)

// TestFix_FileMetaRouteGrouped 钉住 PUT /api/files/meta 属文件组（漏列 ⇒ fileRoute
// fail-closed 500「route not in file group」，实测 sclient meta set 恒 500）。
// 变异：删 isFileGroupedRoute 的 "/api/files/meta" → 本用例红。
func TestFix_FileMetaRouteGrouped(t *testing.T) {
	t.Parallel()
	if !isFileGroupedRoute("/api/files/meta") {
		t.Fatal("PUT /api/files/meta 必须在 isFileGroupedRoute 清单（否则 fileRoute 包装 fail-closed 500）")
	}
	// 写面：不得被判为只读（否则 fileRoute 包装会 500「写包装包了只读子组」）。
	if isReadOnlyFileRoute("/api/files/meta", http.MethodPut) {
		t.Fatal("PUT /api/files/meta 是写面，不得判为只读子组")
	}
}

// TestFix_LocalMuxGateCoversMeta 钉住隧道内层面同源：localMuxGate 对 meta 路由同样收口。
func TestFix_LocalMuxGateCoversMeta(t *testing.T) {
	t.Parallel()
	if !isFileGroupedRoute("/api/files/meta") {
		t.Fatal("localMuxGate 与 isFileGroupedRoute 同源；漏列会让隧道内层面裸放")
	}
}

// TestFix_FileMetaGetRouteGrouped 钉住 GET /api/files/meta 属文件组且为只读子组
// （#855 教训三处同源：isFileGroupedRoute + isReadOnlyFileRoute + HandleFunc）。
// 变异：删 isReadOnlyFileRoute 的 "/api/files/meta" → fileRouteRead 判定「非只读」
// → 接线错 500 → 本用例红。
func TestFix_FileMetaGetRouteGrouped(t *testing.T) {
	t.Parallel()
	if !isFileGroupedRoute("/api/files/meta") {
		t.Fatal("GET /api/files/meta 必须在 isFileGroupedRoute 清单")
	}
	if !isReadOnlyFileRoute("/api/files/meta", http.MethodGet) {
		t.Fatal("GET /api/files/meta 必须在 isReadOnlyFileRoute 只读子组（否则 fileRouteRead 接线错 500）")
	}
}
