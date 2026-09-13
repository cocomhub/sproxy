// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package fsutil 是 `pkg/sync` 及其子包共用的**实现细节工具箱**（路径规范化与 ctx 感知拷贝）。
//
// 放 `pkg/sync/internal/` 而非导出：它们不是领域能力，而是 FS 实现（LocalFS / HTTPTransport）
// 的实现细节；Go 的 internal 规则天然把可见性限定在 `pkg/sync/**` 内，既不污染 pkg/sync 的
// 公开 API，也避免把同一段路径/IO 逻辑复制到两个实现里。
package fsutil

import (
	"context"
	"fmt"
	"io"
	"path"
	"runtime"
	"strings"
)

// SanitizeRelPath 校验并规范化 relPath：
//   - 拒绝空字节、绝对路径（/、\、盘符）、路径穿越（..）、Windows 非法字符
//   - Windows 上把反斜杠归一为正斜杠
//   - 返回正斜杠形式的清洗后相对路径（"" 表示根）
func SanitizeRelPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("路径包含空字节")
	}
	if p == "" {
		return "", nil
	}
	if runtime.GOOS == "windows" {
		p = strings.ReplaceAll(p, "\\", "/")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("路径不能是绝对路径: %s", p)
	}
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("路径不能是绝对路径（盘符）: %s", p)
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "", fmt.Errorf("无效路径: %s", p)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("路径穿越拒绝: %s", p)
	}
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		for _, c := range cleaned {
			if strings.ContainsRune(invalidChars, c) {
				return "", fmt.Errorf("路径包含非法字符 %q: %s", c, p)
			}
		}
	}
	return cleaned, nil
}

// CopyWithCtx 流式拷贝并周期性检查 ctx.Done()，支持大文件传输/哈希的取消
// （审查 I-3：LocalFS 是阻塞 IO，纯 io.Copy 无法在取消时中断；这里每 64KiB 让出一次）。
// 返回已拷贝字节与错误；ctx 取消时返回 ctx.Err()。
func CopyWithCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			total += int64(nw)
			if ew != nil {
				return total, ew
			}
			if nr != nw {
				return total, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return total, nil
			}
			return total, er
		}
	}
}
