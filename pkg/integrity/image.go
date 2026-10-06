// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	"context"
	"image"
	_ "image/gif"  // 注册 GIF 格式
	_ "image/jpeg" // 注册 JPEG 格式
	_ "image/png"  // 注册 PNG 格式
	"os"
	"strings"
)

// init 把 ImageChecker 装配进包级默认注册表（shardseal 模式：Kind 全局唯一，
// 重复注册 fail-fast panic——见 Registry.Register）。
func init() {
	Register("image/*", func() Checker { return ImageChecker{} })
}

// ImageChecker 校验图片语义可用性：文件可被标准库 image 解码且 Bounds 非空。
// 移植 cocom pkg/imaging VerifyImage 语义（DecodeConfig + Decode + Bounds 非空），
// 不引入 cocom 依赖（标准库 image/gif、jpeg、png 空白导入；webp 留后续独立 module）。
type ImageChecker struct{}

// Kind 返回类型标识（注册键，全局唯一）。
func (ImageChecker) Kind() string { return "image/*" }

// Matches 按扩展名族判定归属：.png/.jpg/.jpeg/.gif/.webp/.bmp（不区分大小写）。
func (ImageChecker) Matches(name string) bool {
	lower := strings.ToLower(name)
	// R1-I1：仅匹配标准库 image 有解码器的格式（png/jpg/jpeg/gif）。webp/bmp 无标准库
	// 解码器（需 x/image），声明即误报 damaged（force 下误阻断）→ 不匹配。
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Check 解码 path 指向的图片：解码失败或 Bounds 为空（0x0）→ OK=false（内容异常）；
// 文件打开失败 → error（校验执行错误，非语义判定）。Review Focus 2：空文件/0 字节
// 的图片解码必然失败，判 damaged（字节级校验另管，不冲突）。
func (ImageChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return &Report{OK: false, Reason: err.Error()}, nil
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return &Report{OK: false, Reason: "invalid image bounds"}, nil
	}
	return &Report{OK: true}, nil
}
