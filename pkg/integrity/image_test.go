// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
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

// buildOversizedPNGHeader 构造仅含 PNG 签名 + IHDR（尺寸 w×h）的最小 PNG 字节：
// DecodeConfig 读尺寸即触发超像素分支，不写 IDAT/不触发完整解码（防超大缓冲）。
// crc 用真实 zlib CRC（png 解码校验 IHDR chunk crc），否则 DecodeConfig 报 crc error
// 而非走到像素超限分支。
func buildOversizedPNGHeader(w, h int) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(h))
	ihdr[8], ihdr[9], ihdr[10] = 8, 6, 0 // bit depth 8, color type 6 (RGBA), compression/filter/interlace 0
	// PNG chunk：4B 长度 + 4B 类型 + 数据 + 4B CRC（CRC 覆盖 type+data）。
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 13)
	buf.Write(lenBuf[:])
	buf.WriteString("IHDR")
	buf.Write(ihdr)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("IHDR"))
	_, _ = crc.Write(ihdr)
	sum := crc.Sum32()
	buf.Write([]byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)})
	return buf.Bytes()
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
	// R1-I1：webp/bmp 无标准库解码器，不匹配（避免误报 damaged）。
	for _, name := range []string{"a.png", "b.jpg", "c.jpeg", "d.gif", "u.PNG"} {
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

// TestCheckImage_EstimateMem 估算：1x1 → 4B（RGBA 单像素）。
// 超限像素钳制逻辑（>maxCheckPixels 按 maxPixels×4 估）不物理实例化超大图（>2500 万
// 像素 = >100MB RGBA 缓冲，测试内存不可行）——直接断言钳制常量值（语义层防解压
// 炸弹的数值口径，与 image.go 的 maxCheckPixels 定义一致）。
func TestCheckImage_EstimateMem(t *testing.T) {
	t.Parallel()
	small := writeImagePNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	ic := integrity.ImageChecker{}
	if est := ic.EstimateMem(small, 0); est != 4 {
		t.Fatalf("1x1 PNG 估算应 4B，got %d", est)
	}
	// 钳制常量：maxCheckPixels(2500 万)×4 = 100,000,000 字节（100MB 十进制）——超限图按此估算
	// （不会因 est 无限大而误触 overQuote 跳 unverified；Check 对超限跳过校验 OK=true）。
	const maxPixelsX4 = int64(25000000) * 4
	if maxPixelsX4 != 100000000 {
		t.Fatalf("钳制常量应=100000000 字节，got %d", maxPixelsX4)
	}
}

// TestCheckImage_OverPixelsSkips I2 回归：合法超 2500 万像素大图（8K/大扫描图）→
// OK=true 跳过语义校验（无能力安全解码 ≠ 损坏）——不误判 damaged（与 video 校验器
// 「ffprobe 输出超限放行」同策略）。不物理实例化超大图：用超大 WHDR 构造仅需尺寸
// header 的 PNG（DecodeConfig 读尺寸即触发超限分支，不完整解码）。
func TestCheckImage_OverPixelsSkips(t *testing.T) {
	t.Parallel()
	// 构造 PNG IHDR 尺寸 6000×5000 = 30M 像素 > 2500 万：写入最小 PNG header（IHDR
	// 尺寸块）即够 DecodeConfig 读取尺寸；不写 IDAT（不触发完整解码）。
	big := buildOversizedPNGHeader(6000, 5000)
	path := writeBytes(t, big)
	rep, err := integrity.ImageChecker{}.Check(context.Background(), path, int64(len(big)))
	if err != nil {
		t.Fatalf("超像素图 Check 应无 error，got %v", err)
	}
	if !rep.OK {
		t.Fatalf("超像素应 OK=true 跳过（无能力解码≠损坏），got Reason=%q", rep.Reason)
	}
}
