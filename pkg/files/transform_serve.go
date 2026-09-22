// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// serveTransform 处理 ?transform=<name>&width=N 下载：读原文件 → 按扩展名查注册表
// 变换 → 写派生内容（原文件不动）。无匹配变换 / 变换失败 → 回退原文件（零回归）。
func (s *Service) serveTransform(w http.ResponseWriter, r *http.Request, dp DownloadPath, of OpenedFile, name string) {
	// 查注册表（按原文件扩展名）。
	ext := filepath.Ext(of.Info.Name())
	width := defaultThumbWidth
	if wq := r.URL.Query().Get("width"); wq != "" {
		if n, err := strconv.Atoi(wq); err == nil && n > 0 {
			width = n
		}
	}
	seeker, ok := of.File.(io.ReadSeeker)
	if !ok {
		http.Error(w, "transform: 原文件不支持随机读", http.StatusInternalServerError)
		return
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		s.rt.logger().Warn("transform: seek 原文件失败", "file", dp.Filename, "error", err)
		http.Error(w, "transform failed", http.StatusInternalServerError)
		return
	}
	// 派生缓存（meta/transform/）：键含 rel+checksum+mtime+size+参数——原文件变化即失效。
	key := transformCacheKey(dp.Rel, of.Checksum, of.Info.Size(), of.Info.ModTime().UnixNano(), name, width)
	if cf, ok := loadTransformCache(dp.Tenant, key); ok {
		defer cf.Close()
		// 缓存文件头 8 字节存派生长度 + 类型（写入时防串扰：读不到则回退生成）。
		var hdr [8]byte
		if n, err := io.ReadFull(cf, hdr[:]); err == nil && n == 8 {
			size := int64(hdr[0])<<56 | int64(hdr[1])<<48 | int64(hdr[2])<<40 | int64(hdr[3])<<32 |
				int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
			ct := "image/jpeg" // 内建缩略图固定 JPEG；注册表类型未来扩展时写文件扩展名
			if size >= 0 {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
				if _, err := io.Copy(w, cf); err != nil {
					s.rt.logger().Warn("transform 缓存写出失败", "file", dp.Filename, "error", err)
				}
				return
			}
		}
		// 缓存损坏：删掉回退生成。
		_ = os.Remove(transformCachePath(dp.Tenant, key))
	}
	out, _, ct, err := applyTransform(r.Context(), ext, of.File, of.Info.Size(), name, width)
	if err != nil {
		// 无匹配/失败：回退原文件（ServeContent 路径）。
		s.rt.logger().Debug("transform 回退原文件", "file", dp.Filename, "ext", ext, "error", err)
		http.ServeContent(w, r, of.Info.Name(), of.Info.ModTime(), seeker)
		return
	}
	data, err := io.ReadAll(out)
	if err != nil {
		s.rt.logger().Warn("transform 读取失败", "file", dp.Filename, "error", err)
		http.Error(w, "transform failed", http.StatusInternalServerError)
		return
	}
	// 原子写缓存（头 8 字节 = 派生大小；失败静默——缓存是加速层）。
	if dp.Tenant != nil {
		hdr := make([]byte, 8, 8+len(data))
		n := int64(len(data))
		for i := 7; i >= 0; i-- {
			hdr[i] = byte(n & 0xff)
			n >>= 8
		}
		hdr = append(hdr, data...)
		storeTransformCache(dp.Tenant, key, hdr)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	if _, err := w.Write(data); err != nil {
		s.rt.logger().Warn("transform 写出失败", "file", dp.Filename, "error", err)
	}
}

// applyTransform 按扩展名查注册表并应用。注册表存默认宽度闭包；width 参数不同时
// 直接调 thumbnailTransform（包内可访问，动态宽度）。未知 name/无注册 → 回退原文件。
func applyTransform(ctx context.Context, ext string, src io.Reader, size int64, name string, width int) (io.Reader, int64, string, error) {
	if name != "thumb" {
		return nil, 0, "", fmt.Errorf("transform: 未知变换 %q", name)
	}
	if _, ok := lookupTransform(ext); !ok {
		return nil, 0, "", fmt.Errorf("transform: 扩展名 %q 无注册变换", ext)
	}
	if width <= 0 || width == defaultThumbWidth {
		fn, _ := lookupTransform(ext)
		return fn(ctx, src, size)
	}
	return thumbnailTransform(ctx, src, size, width)
}
