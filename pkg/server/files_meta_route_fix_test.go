// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/router"
)

// TestFix_FileMetaRouteGrouped 钉住 PUT /api/files/meta 声明为文件写子组（错 ⇒ fileRoute
// 接线自检 fail-closed 500，实测 sclient meta set 恒 500）。
// 变异：表里把它改成 ClassReader 或删掉 → 本用例红。
func TestFix_FileMetaRouteGrouped(t *testing.T) {
	t.Parallel()
	if c, ok := fileDomainClass("PUT /api/files/meta"); !ok || c != router.ClassUser {
		t.Fatalf("PUT /api/files/meta 分类 = %v(ok=%v), want ClassUser（否则 fileRoute 自检 500）", c, ok)
	}
}

// TestFix_LocalMuxGateCoversMeta 钉住隧道内层面同源：localMuxGate 查同一张表。
func TestFix_LocalMuxGateCoversMeta(t *testing.T) {
	t.Parallel()
	if _, ok := fileDomainClass("PUT /api/files/meta"); !ok {
		t.Fatal("localMuxGate 与 fileRoute 同源查表；缺声明会让隧道内层裸放")
	}
}

// TestFix_FileMetaGetRouteGrouped 钉住 GET /api/files/meta 声明为只读子组（错 ⇒
// fileRouteRead 接线自检 500）。
// 变异：表里改成 ClassUser → 本用例红。
func TestFix_FileMetaGetRouteGrouped(t *testing.T) {
	t.Parallel()
	if c, ok := fileDomainClass("GET /api/files/meta"); !ok || c != router.ClassReader {
		t.Fatalf("GET /api/files/meta 分类 = %v(ok=%v), want ClassReader（否则 fileRouteRead 自检 500）", c, ok)
	}
}
