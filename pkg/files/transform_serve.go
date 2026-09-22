// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
	out, outSize, ct, err := applyTransform(r.Context(), ext, of.File, of.Info.Size(), name, width)
	if err != nil {
		// 无匹配/失败：回退原文件（ServeContent 路径）。
		s.rt.logger().Debug("transform 回退原文件", "file", dp.Filename, "ext", ext, "error", err)
		http.ServeContent(w, r, of.Info.Name(), of.Info.ModTime(), seeker)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", outSize))
	if _, err := io.Copy(w, out); err != nil {
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
