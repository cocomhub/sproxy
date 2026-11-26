// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShareAccess_TransformThumbnail 锁分享访问的 `?transform=` 语义（**实测缺陷 2026-11-25**）：
//
// 旧实现 `streamShareFile` 直接 `io.Copy` 原文件 ⇒ `?transform=thumb` 被**完全忽略**，
// 客户端拿到原图且 Content-Type 是 application/octet-stream（文档声称的缩略图/水印能力
// 在分享面从未生效）。修复后：分享访问复用 `files.Service` 的派生管线
// （缩略图/水印 + 派生缓存），图片请求应返回 image/* 且体积小于原图。
// tinyPNG 生成一张 512x512 的高熵 PNG（纯标准库）——噪声内容保证「缩小后体积明显变小」，
// 从而能让断言区分「真的做了缩放」与「原样返回」。
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	const side = 512
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	rnd := rand.New(rand.NewSource(42)) // 固定种子：测试可复现
	for y := range side {
		for x := range side {
			img.Set(x, y, color.RGBA{R: uint8(rnd.Intn(256)), G: uint8(rnd.Intn(256)), B: uint8(rnd.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestShareAccess_TransformThumbnail(t *testing.T) {
	t.Parallel()
	baseURL, h := newEventTestServer(t, nil)
	client := testHTTPClient(t)
	png := tinyPNG(t)
	if st, resp := uploadFile(t, baseURL, "pic.png", png, map[string]string{
		"X-File-Checksum": sha256hex(png),
	}); st != http.StatusOK {
		t.Fatalf("上传失败 status=%d body=%s", st, resp)
	}

	create := func(payload string) string {
		resp, err := client.Post(baseURL+"/api/shares", contentTypeJSON, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("create share: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
			t.Fatalf("解析: %v", derr)
		}
		return out.Token
	}

	tok := create(`{"filename":"pic.png","ttl":"1h"}`)
	if tok == "" {
		t.Fatal("未拿到 token")
	}

	// 原文件：octet-stream（内容原样）。
	req := httptest.NewRequest(http.MethodGet, "/s/"+tok, nil)
	req.SetPathValue("token", tok)
	rec := httptest.NewRecorder()
	h.accessShareHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("原文件访问 status=%d", rec.Code)
	}
	orig := rec.Body.Bytes()
	if len(orig) != len(png) {
		t.Fatalf("原文件内容长度 = %d，期望 %d", len(orig), len(png))
	}

	// 缩略图：必须走派生（image/*，且不超过原图）。
	tok2 := create(`{"filename":"pic.png","ttl":"1h"}`)
	req2 := httptest.NewRequest(http.MethodGet, "/s/"+tok2+"?transform=thumb&width=8", nil)
	req2.SetPathValue("token", tok2)
	rec2 := httptest.NewRecorder()
	h.accessShareHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("缩略图访问 status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	ct := rec2.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		t.Fatalf("缩略图 Content-Type = %q，期望 image/*（transform 被忽略？）", ct)
	}
	if rec2.Body.Len() > len(png) {
		t.Fatalf("缩略图体积 %d > 原图 %d（未实际缩放）", rec2.Body.Len(), len(png))
	}

	// 水印：分享绑定的种子在响应头可见；带 watermark 参数的派生也应是 image/*。
	tok3 := create(`{"filename":"pic.png","ttl":"1h","watermark":"seed1"}`)
	req3 := httptest.NewRequest(http.MethodGet, "/s/"+tok3+"?transform=thumb&watermark=seed1", nil)
	req3.SetPathValue("token", tok3)
	rec3 := httptest.NewRecorder()
	h.accessShareHandler(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("水印访问 status=%d", rec3.Code)
	}
	if got := rec3.Header().Get("X-Share-Watermark"); got != "seed1" {
		t.Fatalf("X-Share-Watermark = %q，期望 seed1", got)
	}
	if !strings.HasPrefix(rec3.Header().Get("Content-Type"), "image/") {
		t.Fatalf("水印 Content-Type = %q，期望 image/*", rec3.Header().Get("Content-Type"))
	}
	_, _ = io.Copy(io.Discard, rec3.Body)
}
