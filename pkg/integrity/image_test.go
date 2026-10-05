// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// writeZeroPNG 手工构造一个编码层合法的 0x0 PNG（IHDR 宽高为 0）：
// image/png 编码器拒绝 0x0 输入，需直接拼字节；CRC 按 PNG 规范计算。
func writeZeroPNG(t *testing.T) string {
	t.Helper()
	drawData := []byte{
		0x00, 0x00, 0x00, 0x0d, // 长度 13
		'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x00, // 宽 0
		0x00, 0x00, 0x00, 0x00, // 高 0
		0x08, 0x06, 0x00, 0x00, 0x00, // 8bit RGBA
		0x3b, 0x8b, 0x7c, 0x12, // IHDR CRC（含类型字节）
		0x00, 0x00, 0x00, 0x00, // 长度 0
		'I', 'E', 'N', 'D',
		0xae, 0x42, 0x60, 0x82, // IEND CRC
	}
	path := filepath.Join(t.TempDir(), "zero.png")
	if err := os.WriteFile(path, append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, drawData...), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// writeImagePNG 把 img 编码为 PNG 落到临时文件，返回路径。
func writeImagePNG(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	path := filepath.Join(t.TempDir(), "img.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// writeBytes 写原始字节到临时文件。
func writeBytes(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestCheckImage_ValidPNG：1x1 有效 PNG（image/png 编码）应 OK。
func TestCheckImage_ValidPNG(t *testing.T) {
	t.Parallel()
	path := writeImagePNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	rep, err := integrity.ImageChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("有效 PNG 不应返回 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("有效 PNG 应 OK，got Reason=%q", rep.Reason)
	}
}

// TestCheckImage_Corrupt：非图片字节应失败（不回 error，仅 OK=false）。
func TestCheckImage_Corrupt(t *testing.T) {
	t.Parallel()
	path := writeBytes(t, []byte("not-an-image"))
	rep, err := integrity.ImageChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("损坏图片应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("损坏图片应失败")
	}
}

// TestCheckImage_ZeroBounds：0x0 PNG（编码合法但 Bounds 空）应失败（Review Focus 2）。
func TestCheckImage_ZeroBounds(t *testing.T) {
	t.Parallel()
	path := writeZeroPNG(t)
	rep, err := integrity.ImageChecker{}.Check(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("0x0 图片应返回 OK=false 语义而非 error，got %v", err)
	}
	if rep.OK {
		t.Fatal("0x0 图片应失败（Bounds 空）")
	}
}

// TestImageChecker_Matches：扩展名族匹配（设计 §77 规则）。
func TestImageChecker_Matches(t *testing.T) {
	t.Parallel()
	c := integrity.ImageChecker{}
	for _, name := range []string{"a.png", "b.jpg", "c.jpeg", "d.gif", "e.webp", "f.bmp", "u.PNG"} {
		if !c.Matches(name) {
			t.Errorf("Matches(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"x.txt", "y.tar", "z.tar.gz"} {
		if c.Matches(name) {
			t.Errorf("Matches(%q) = true, want false", name)
		}
	}
}

// TestImageChecker_Kind：类型标识为注册键。
func TestImageChecker_Kind(t *testing.T) {
	t.Parallel()
	if (integrity.ImageChecker{}).Kind() != "image/*" {
		t.Fatal("Kind 应为 image/*")
	}
}
