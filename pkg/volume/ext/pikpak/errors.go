// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// errors.go 定义 pikpak 模块的哨兵错误与辅助函数。

import (
	"errors"
	"io"
	"os"
	"unicode/utf8"
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
	// ErrNoAccountAvailable 表示账号池里没有剩余配额满足需求的可用账号。
	ErrNoAccountAvailable = errors.New("pikpak: no account available with enough quota")
	// ErrDuplicateAccount 表示账号名重复（账号名唯一）。
	ErrDuplicateAccount = errors.New("pikpak: duplicate account name")
	// ErrAccountNotFound 表示账号不存在。
	ErrAccountNotFound = errors.New("pikpak: account not found")
)

// truncate 截断字符串（日志/错误信息），按 rune 截断避免劈开 UTF-8 多字节字符
// （中文/emoji 错误文案不被截成非法编码，满足 UTF-8 无 BOM 编码纪律）。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// 从 limit 字节位置向前退到合法 rune 边界。
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	if end == 0 {
		// 首字节就是半个 rune（极端情况）：返回空，保留完整语义的最小截断。
		return ""
	}
	return s[:end]
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
