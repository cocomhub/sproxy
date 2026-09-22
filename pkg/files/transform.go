// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"maps"
	"strings"
	"sync"
)

// TransformFunc 是上传管线变换函数：把 src（原文件内容）变换为派生内容。
// 返回派生内容的 Reader + 大小 + Content-Type；错误时返回 error（调用方回退原文件）。
//
// 语义（roadmap 2.3 P2）：
//   - 原文件不动——transform 是派生物（下载时按需生成 + 缓存），绝不写回原路径；
//   - 纯标准库（image 等），不引第三方。
type TransformFunc func(ctx context.Context, src io.Reader, size int64) (io.Reader, int64, string, error)

// transformRegistry 是包级变换注册表（扩展名 → 处理函数），与 plugin.Registry 同构：
// 内建缩略图默认注册；装配层可追加（压缩/转码等）。
var transformRegistry = struct {
	mu sync.RWMutex
	m  map[string]TransformFunc
}{m: make(map[string]TransformFunc)}

// RegisterTransform 按扩展名注册变换函数（如 ".jpg"）。返回 true = 新注册；false = 已存在（不覆盖）。
func RegisterTransform(ext string, fn TransformFunc) bool {
	if fn == nil {
		return false
	}
	ext = normalizeTransformExt(ext)
	transformRegistry.mu.Lock()
	defer transformRegistry.mu.Unlock()
	if _, ok := transformRegistry.m[ext]; ok {
		return false
	}
	transformRegistry.m[ext] = fn
	return true
}

// lookupTransform 按扩展名查询变换函数（ok=false = 未注册）。
func lookupTransform(ext string) (TransformFunc, bool) {
	ext = normalizeTransformExt(ext)
	transformRegistry.mu.RLock()
	defer transformRegistry.mu.RUnlock()
	fn, ok := transformRegistry.m[ext]
	return fn, ok
}

// normalizeTransformExt 归一扩展名（小写 + 补前导点）。
func normalizeTransformExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// init 自动注册内建变换（图片缩略图）——装配层无需显式调用；幂等安全。
func init() { RegisterBuiltinTransforms() }

// RegisterBuiltinTransforms 注册内建变换（图片缩略图：.jpg/.jpeg/.png/.gif → thumbnailTransform）。
// 幂等（重复调用不覆盖既有注册）。装配层可显式调用。
func RegisterBuiltinTransforms() {
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif"} {
		RegisterTransform(ext, func(ctx context.Context, src io.Reader, size int64) (io.Reader, int64, string, error) {
			return thumbnailTransform(ctx, src, size, defaultThumbWidth)
		})
	}
}

// defaultThumbWidth 是缩略图默认目标宽度（px）；高度按比例缩放。
const defaultThumbWidth = 256

// thumbnailTransform 生成指定宽度的 JPEG 缩略图（高度按原比例）。
// 输入非图片 / 解码失败 → error（调用方回退原文件）。
func thumbnailTransform(ctx context.Context, src io.Reader, size int64, width int) (io.Reader, int64, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, "", err
	}
	if width <= 0 {
		width = defaultThumbWidth
	}
	img, _, err := image.Decode(src)
	if err != nil {
		return nil, 0, "", err
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, 0, "", errInvalidTransformImage
	}
	// 等比例缩放：目标宽度 = width，高度按原宽高比。
	height := int(float64(b.Dy()) * float64(width) / float64(b.Dx()))
	if height <= 0 {
		height = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	// 邻近采样缩放（标准库 image/draw 无缩放；逐像素从原图最近点取色）。
	srcW, srcH := b.Dx(), b.Dy()
	for y := 0; y < height; y++ {
		sy := y * srcH / height
		if sy >= srcH {
			sy = srcH - 1
		}
		for x := 0; x < width; x++ {
			sx := x * srcW / width
			if sx >= srcW {
				sx = srcW - 1
			}
			dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}

	// 编码为 JPEG（bytes.Buffer——size 可得，响应 Content-Length 准确）。
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, nil); err != nil {
		return nil, 0, "", err
	}
	return bytes.NewReader(buf.Bytes()), int64(buf.Len()), "image/jpeg", nil
}

// errInvalidTransformImage 是无效图片错误（回退原文件用）。
var errInvalidTransformImage = errTransform("invalid image")

// errTransform 是变换错误包装。
type errTransform string

func (e errTransform) Error() string { return string(e) }

// ---- 测试隔离辅助 ----

// transformRegistrySnapshot 返回当前注册表快照（测试隔离恢复用）。
func transformRegistrySnapshot() map[string]TransformFunc {
	transformRegistry.mu.RLock()
	defer transformRegistry.mu.RUnlock()
	out := make(map[string]TransformFunc, len(transformRegistry.m))
	maps.Copy(out, transformRegistry.m)
	return out
}

// transformRegistryClear 清空注册表（测试隔离）。
func transformRegistryClear() {
	transformRegistry.mu.Lock()
	defer transformRegistry.mu.Unlock()
	transformRegistry.m = make(map[string]TransformFunc)
}

// transformRegistryRestore 恢复注册表快照（测试隔离）。
func transformRegistryRestore(snapshot map[string]TransformFunc) {
	transformRegistry.mu.Lock()
	defer transformRegistry.mu.Unlock()
	transformRegistry.m = make(map[string]TransformFunc, len(snapshot))
	maps.Copy(transformRegistry.m, snapshot)
}

// ensurePNGImport 引用 image/png（内建装配用到；防 gofmt 删 import）。
var _ = png.Decode
