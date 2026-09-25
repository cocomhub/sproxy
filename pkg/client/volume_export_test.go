// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// volume_export_test.go 覆盖 FileClient.ExportVolume（sclient backup CLI 的客户端面）：
//   - GET /api/volumes/export?volume=<name> → 流式 tar 落盘（原子 tmp+Rename）；
//   - 服务端错误 → 报错且不残留半成品文件；
//   - 输出路径穿越 / 空卷名校验（fail-closed，不发请求）。
//
// 约束：纯标准库断言；httptest（127.0.0.1 回环）；NewFileClient 自带独立连接池
// （不共享 http.DefaultClient/Transport——硬规则）。

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exportTestTar 构造一个最小合法 tar（user/a.txt + 尾部 manifest.json），供 mock 返回。
func exportTestTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "user/a.txt", Mode: 0o644, Size: 5}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("写 tar 头失败: %v", err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatalf("写 tar 内容失败: %v", err)
	}
	manifest := []byte(`{"volume":"disk2","files":[{"path":"user/a.txt","size":5}]}`)
	mhdr := &tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}
	if err := tw.WriteHeader(mhdr); err != nil {
		t.Fatalf("写 manifest 头失败: %v", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatalf("写 manifest 内容失败: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// TestClient_ExportVolume_Success 断言：请求 GET /api/volumes/export?volume=<name>，
// 响应流式写入本地文件且内容与响应体一致（原子落盘，无 .tmp 残留）。
func TestClient_ExportVolume_Success(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	tarBytes := exportTestTar(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("export method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/volumes/export" {
			t.Errorf("export path = %s, want /api/volumes/export", r.URL.Path)
		}
		if got := r.URL.Query().Get("volume"); got != "disk2" {
			t.Errorf("export volume query = %q, want disk2", got)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	outPath := filepath.Join(t.TempDir(), "disk2.tar")
	c := NewFileClient(srv.URL)
	if err := c.ExportVolume(context.Background(), "disk2", outPath); err != nil {
		t.Fatalf("ExportVolume failed: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读导出文件失败: %v", err)
	}
	if !bytes.Equal(got, tarBytes) {
		t.Errorf("导出文件内容与响应体不一致（len got=%d want=%d）", len(got), len(tarBytes))
	}
	if _, err := os.Stat(outPath + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("原子落盘后不应残留 .tmp（Stat err=%v）", err)
	}
}

// TestClient_ExportVolume_EmptyVolumeOmitsQuery 断言：vol 为空（全卷视图）时请求
// 不携带 volume 参数（服务端导出全部可见卷——与 download 的 auto 语义一致）。
func TestClient_ExportVolume_EmptyVolumeOmitsQuery(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("volume"); got != "" {
			t.Errorf("空卷导出不应携带 volume 参数，got %q", got)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(exportTestTar(t))
	}))
	defer srv.Close()

	outPath := filepath.Join(t.TempDir(), "all.tar")
	c := NewFileClient(srv.URL)
	if err := c.ExportVolume(context.Background(), "", outPath); err != nil {
		t.Fatalf("ExportVolume(空卷) failed: %v", err)
	}
}

// TestClient_ExportVolume_ServerError 断言：服务端 500 → 返回错误，且不残留半成品文件。
func TestClient_ExportVolume_ServerError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "volume not allowed", http.StatusForbidden)
	}))
	defer srv.Close()

	outPath := filepath.Join(t.TempDir(), "disk2.tar")
	c := NewFileClient(srv.URL)
	err := c.ExportVolume(context.Background(), "disk2", outPath)
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	if _, serr := os.Stat(outPath); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("导出失败不应留下目标文件（Stat err=%v）", serr)
	}
	if entries, rerr := os.ReadDir(filepath.Dir(outPath)); rerr == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") {
				t.Errorf("导出失败不应残留 .tmp 文件: %s", e.Name())
			}
		}
	}
}

// TestClient_ExportVolume_PathTraversal 断言：输出路径含 .. → 客户端 fail-closed 拒绝，
// 不发出请求（零请求计数佐证）。
func TestClient_ExportVolume_PathTraversal(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	err := c.ExportVolume(context.Background(), "disk2", filepath.Join("..", "..", "escape.tar"))
	if err == nil {
		t.Fatal("路径穿越输出应被拒绝")
	}
	if !strings.Contains(err.Error(), "路径穿越") {
		t.Errorf("错误信息应说明路径穿越，got: %v", err)
	}
	if requests != 0 {
		t.Errorf("路径穿越校验应在发请求前拦截（requests=%d）", requests)
	}
}

// TestClient_ExportVolume_EmptyOutputPath 断言：空输出路径 → 参数校验错误。
func TestClient_ExportVolume_EmptyOutputPath(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	c := NewFileClient("http://127.0.0.1:1")
	if err := c.ExportVolume(context.Background(), "disk2", ""); err == nil {
		t.Fatal("空输出路径应报错")
	}
}

// TestClient_ExportVolume_TunnelReady 断言：请求复用 doRequest（签名/隧道管线）——
// 通过注入自定义 RequestSigner 观察其被调用（证明 ExportVolume 走既有认证管线而非裸 http）。
func TestClient_ExportVolume_TunnelReady(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	signed := false
	signer := &fakeSigner{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Custom-Signer"); got == "" {
			t.Error("导出请求未携带签名头（走裸 http 而非 doRequest）")
		}
		if r.URL.Path != "/api/volumes/export" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(exportTestTar(t))
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithRequestSigner(signer))
	if err := c.ExportVolume(context.Background(), "disk2", filepath.Join(t.TempDir(), "out.tar")); err != nil {
		t.Fatalf("ExportVolume failed: %v", err)
	}
	if signer.calls.Load() == 0 {
		t.Error("ExportVolume 未调用注入的 RequestSigner（应走既有签名管线）")
	}
	_ = signed
}
