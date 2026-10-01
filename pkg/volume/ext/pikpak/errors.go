// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// errors.go 定义 pikpak 模块的哨兵错误与辅助函数。

import (
	"errors"
	"io"
	"os"
)

// 哨兵错误（错误处理优先哨兵 + %w 包装，跨包用 errors.Is）。
var (
	// ErrNotLoggedIn 表示 CLI 未登录（需先 OAuth 授权）。
	ErrNotLoggedIn = errors.New("pikpak: not logged in")
	// ErrShareNotFound 表示分享链接无法解析/已失效。
	ErrShareNotFound = errors.New("pikpak: share not found")
	// ErrFileNotFound 表示网盘中找不到目标文件。
	ErrFileNotFound = errors.New("pikpak: file not found")
	// ErrUnsupported 表示不支持的 URL 形态。
	ErrUnsupported = errors.New("pikpak: unsupported url")
)

// truncate 截断字符串（日志/错误信息）。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ioCopy 是 io.Copy 的薄封装（减少 import 面）。
func ioCopy(dst io.Writer, src io.Reader) (int64, error) {
	return copyStream(dst, src)
}

// copyStream 复制流。
func copyStream(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			wn, werr := dst.Write(buf[:n])
			total += int64(wn)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// fileExists 判断路径是否为存在的常规文件。
func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
