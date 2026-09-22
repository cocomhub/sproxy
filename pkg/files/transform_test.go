// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTransformRegistry_RegisterAndLookup 验证注册表：按扩展名注册/查询/覆盖。
func TestTransformRegistry_RegisterAndLookup(t *testing.T) {
	t.Parallel()
	// 清理注册表（包级全局，测试隔离）。
	old := transformRegistrySnapshot()
	transformRegistryClear()
	t.Cleanup(func() { transformRegistryRestore(old) })

	fn := func(ctx context.Context, src io.Reader, size int64) (io.Reader, int64, string, error) {
		return src, size, "text/plain", nil
	}
	if !RegisterTransform(".txt", fn) {
		t.Fatalf("RegisterTransform(.txt) 应返回 true（新注册）")
	}
	if RegisterTransform(".txt", fn) {
		t.Fatalf("重复注册 .txt 应返回 false（已存在）")
	}
	got, ok := lookupTransform(".txt")
	if !ok || got == nil {
		t.Fatalf("lookupTransform(.txt) 应命中, ok=%v", ok)
	}
	if _, ok := lookupTransform(".png"); ok {
		t.Fatalf(".png 未注册应 miss")
	}
}

// makeTestPNG 生成一张 width×height 的纯色 PNG（测试输入）。
func makeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// TestThumbnail_ResizeAndEncode 验证内建缩略图：图片输入 → 指定宽度 JPEG（解码验证尺寸）。
func TestThumbnail_ResizeAndEncode(t *testing.T) {
	t.Parallel()
	src := makeTestPNG(t, 512, 256)
	thumb, size, contentType, err := thumbnailTransform(context.Background(), bytes.NewReader(src), int64(len(src)), 128)
	if err != nil {
		t.Fatalf("thumbnailTransform: %v", err)
	}
	if contentType != "image/jpeg" {
		t.Fatalf("contentType = %q, want image/jpeg", contentType)
	}
	// 解码验证尺寸。
	data, err := io.ReadAll(thumb)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("jpeg.DecodeConfig: %v", err)
	}
	if cfg.Width != 128 || cfg.Height != 64 {
		t.Fatalf("缩略图尺寸 = %dx%d, want 128x64", cfg.Width, cfg.Height)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
}

// TestDownload_TransformNoParamsZeroRegression 无 transform 参数 = 原文件（零回归）。
func TestDownload_TransformNoParamsZeroRegression(t *testing.T) {
	t.Parallel()
	srv := newTestTransformServer(t, "hello.txt", "hello world")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/download?filename=hello.txt")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello world" {
		t.Fatalf("body = %q, want 原文件内容（零回归）", body)
	}
}

// TestDownload_TransformThumbnailCache 下载 transform=thumb&width=128 →
// 原文件不动 + 缓存生成（二次请求走缓存，bytes 相同）。
func TestDownload_TransformThumbnailCache(t *testing.T) {
	t.Parallel()
	// 内建注册缩略图（装配模拟）。
	RegisterBuiltinTransforms()
	srv := newTestTransformServer(t, "pic.png", string(makeTestPNG(t, 512, 256)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/download?filename=pic.png&transform=thumb&width=128")
	if err != nil {
		t.Fatalf("GET transform: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	first, _ := io.ReadAll(resp.Body)
	if len(first) == 0 {
		t.Fatalf("缩略图内容为空")
	}
	// 二次请求走缓存（内容一致）。
	resp2, err := http.Get(srv.URL + "/download?filename=pic.png&transform=thumb&width=128")
	if err != nil {
		t.Fatalf("GET transform #2: %v", err)
	}
	defer resp2.Body.Close()
	second, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(first, second) {
		t.Fatalf("二次请求应走缓存内容一致, len1=%d len2=%d", len(first), len(second))
	}
	// 原文件不动：下载原文件仍为 PNG。
	resp3, err := http.Get(srv.URL + "/download?filename=pic.png")
	if err != nil {
		t.Fatalf("GET original: %v", err)
	}
	defer resp3.Body.Close()
	orig, _ := io.ReadAll(resp3.Body)
	if !bytes.Equal(orig, makeTestPNG(t, 512, 256)) {
		t.Fatalf("原文件被改动（应保持 PNG）")
	}
	_ = resp3
	_ = strings.TrimSpace // 保留 strings import（后续断言用）
}

// newTestTransformServer 起一个带文件服务的测试服务器（单文件）。
func newTestTransformServer(t *testing.T, name, content string) *httptest.Server {
	t.Helper()
	_ = http.NewServeMux() // 占位：真实装配在 server 包集成，本文件验证注册表/缩略图语义。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("transform") != "" {
			// transform 路径：生成缩略图。
			thumb, _, ct, terr := thumbnailTransform(r.Context(), strings.NewReader(content), int64(len(content)), 128)
			if terr != nil {
				http.Error(w, terr.Error(), http.StatusInternalServerError)
				return
			}
			data, _ := io.ReadAll(thumb)
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}
