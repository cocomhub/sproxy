// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// ValidateFilePath 校验并规范化用户提供的文件路径（可能包含子目录）。
// 返回使用平台分隔符的清洗后相对路径，或描述性错误。
//
// 规则：
//   - 拒绝空字符串
//   - 拒绝空字节（\x00）
//   - 拒绝绝对路径（以 / 或 \ 开头）
//   - filepath.Clean 规范化
//   - 逐组件检查 ".."（路径穿越）
//   - Windows 上检查 <>:"|?* 非法字符
//   - 返回路径为 filepath.ToSlash 格式（使用 / 分隔符），适合作为 API 返回值
func ValidateFilePath(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("文件名不能为空")
	}

	// 拒绝空字节
	if strings.ContainsRune(filename, '\x00') {
		return "", fmt.Errorf("文件名包含空字节")
	}

	// 拒绝绝对路径（以 / 或 \ 开头）
	if filename[0] == '/' || filename[0] == '\\' {
		return "", fmt.Errorf("文件名不能是绝对路径: %s", filename)
	}

	// 清理路径
	cleaned := filepath.Clean(filename)
	if cleaned == "." {
		return "", fmt.Errorf("无效的文件名: %s", filename)
	}

	// Clean 后再次检查绝对路径（Windows 上如 C:\ 会在 Clean 后才被 IsAbs 捕获）
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("文件名不能是绝对路径: %s", filename)
	}

	// 逐组件检查 ".."（路径穿越）
	parts := strings.Split(cleaned, string(filepath.Separator))
	if slices.Contains(parts, "..") {
		return "", fmt.Errorf("文件名不能包含路径穿越: %s", filename)
	}

	// Windows 非法字符检查
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		for _, c := range filename {
			if strings.ContainsRune(invalidChars, c) {
				return "", fmt.Errorf("文件名包含非法字符 %q: %s", c, filename)
			}
		}
	}

	// 统一分隔符为 / 用于 API 序列化
	return filepath.ToSlash(cleaned), nil
}
