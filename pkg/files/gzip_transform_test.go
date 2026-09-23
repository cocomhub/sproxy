// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// gzip_transform_test.go 验证 gzip 压缩变换（roadmap P2 服务端压缩插件）：
//  1. ?transform=gzip 下载 → 响应 Content-Encoding: gzip + gzip 解码后内容 = 原文件。
//  2. 未注册扩展名（.bin）→ 回退原文件（零回归）。

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeTestFile 直接写文件到 alice 租户 user 桶（dirsEnv 单卷无 routeUpload，
// WriteFile 域 API 会走 nil tenant panic——写面路由由 pkg/server 侧测试覆盖）。
func writeTestFile(t *testing.T, env *dirsEnv, rel, content string) {
	t.Helper()
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant alice 不可用")
	}
	userAbs, ok := tnt.Root().Abs("user")
	if !ok {
		t.Fatal("user 桶不可用")
	}
	full := filepath.Join(userAbs, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGzipTransform_Download ?transform=gzip 下载文本 → gzip 解码 = 原文。
func TestGzipTransform_Download(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		tnt := env.tenantFor("alice")
		return DownloadPath{Tenant: tnt, Rel: "user/docs.txt", Filename: "docs.txt"}, nil
	}
	env.rebuild()
	writeTestFile(t, env, "docs.txt", "hello gzip world")
	req := httptest.NewRequest(http.MethodGet, "/download?filename=docs.txt&transform=gzip", nil)
	w := httptest.NewRecorder()
	env.svc.Download(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET transform=gzip = %d, want 200", w.Code)
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", w.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	got, _ := io.ReadAll(zr)
	if string(got) != "hello gzip world" {
		t.Fatalf("gzip 解码 = %q, want 原文", got)
	}
}

// TestGzipTransform_ThumbUnregisteredExt ?transform=thumb 对无注册扩展名回退原文件。
// （gzip 是命名变换与扩展名无关；未注册扩展名回退只针对 thumb 等按扩展名查表的变换。）
func TestGzipTransform_ThumbUnregisteredExt(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		tnt := env.tenantFor("alice")
		return DownloadPath{Tenant: tnt, Rel: "user/data.bin", Filename: "data.bin"}, nil
	}
	env.rebuild()
	writeTestFile(t, env, "data.bin", "\x00\x01\x02")
	req := httptest.NewRequest(http.MethodGet, "/download?filename=data.bin&transform=thumb", nil)
	w := httptest.NewRecorder()
	env.svc.Download(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}
	if w.Header().Get("Content-Encoding") == "gzip" {
		t.Fatalf(".bin thumb 不应 gzip")
	}
}
