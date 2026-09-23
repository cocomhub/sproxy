// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// watermark_test.go 验证分享图片水印（roadmap P2 分享权限细化残余）：
//  1. ?transform=thumb&watermark=<seed> 下载 → 像素与无水印不同（水印叠加）。
//  2. 无水印参数 → 普通缩略图（零回归）。

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWatermark_ChangesPixels 水印变换 → 像素与无水印不同。
func TestWatermark_ChangesPixels(t *testing.T) {
	t.Parallel()
	src := makeTestPNG(t, 200, 100)
	// 无水平铺（对照）。
	plain, _, _, err := thumbnailTransform(context.Background(), bytes.NewReader(src), int64(len(src)), 200)
	if err != nil {
		t.Fatalf("thumb: %v", err)
	}
	plainData, _ := io.ReadAll(plain)
	// 水印变换。
	wm, _, _, err := watermarkTransform(context.Background(), bytes.NewReader(src), int64(len(src)), 200, "sproxy-share")
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	wmData, _ := io.ReadAll(wm)
	if bytes.Equal(plainData, wmData) {
		t.Fatal("水印变换后像素应与无水印不同")
	}
	// 两者都可解码为 JPEG。
	if _, err := jpeg.Decode(bytes.NewReader(wmData)); err != nil {
		t.Fatalf("水印输出应为 JPEG: %v", err)
	}
}

// TestWatermark_NoParam_ZeroRegression 无 watermark 参数 → 普通缩略图（零回归）。
func TestWatermark_NoParam_ZeroRegression(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		tnt := env.tenantFor("alice")
		return DownloadPath{Tenant: tnt, Rel: "user/pic.png", Filename: "pic.png"}, nil
	}
	env.rebuild()
	writeTestFile(t, env, "pic.png", string(makeTestPNG(t, 200, 100)))
	req := httptest.NewRequest(http.MethodGet, "/download?filename=pic.png&transform=thumb&width=200", nil)
	w := httptest.NewRecorder()
	env.svc.Download(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d", w.Code)
	}
	// 无 watermark 参数应正常缩略图（可解码 JPEG）。
	if _, err := jpeg.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatalf("应输出 JPEG: %v", err)
	}
}

var _ = strings.TrimSpace

// TestWatermark_CacheKeyIsolated 钉住「水印缓存隔离」（审查 P1 修复）：
// 带水印与无水印（或不同 seed）的 transformCacheKey 必须不同——否则共享缓存条目
// 导致水印绕过（先缓存无水印图 → 带水印请求命中返回无水印）或反向污染。
func TestWatermark_CacheKeyIsolated(t *testing.T) {
	t.Parallel()
	plain := transformCacheKey("user/a.png", "c1", 10, 100, "thumb", 128, "")
	wm1 := transformCacheKey("user/a.png", "c1", 10, 100, "thumb", 128, "seed-a")
	wm2 := transformCacheKey("user/a.png", "c1", 10, 100, "thumb", 128, "seed-b")
	if plain == wm1 {
		t.Fatal("无水印与带水印缓存键不应相同（水印绕过）")
	}
	if wm1 == wm2 {
		t.Fatal("不同 seed 水印缓存键不应相同")
	}
	if plain == wm2 {
		t.Fatal("无水印与不同 seed 水印缓存键不应相同")
	}
}
