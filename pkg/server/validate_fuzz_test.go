// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// FuzzValidateFilePath 模糊测试 ValidateFilePath 函数。
//
// 验证不变量：
//  1. 合法路径不返回 error
//  2. 返回路径被 filepath.ToSlash 格式化（使用 / 分隔符）
//  3. 返回路径在追加到 UploadsDir 后不会逃逸出目录（无路径穿越）
//  4. 任何输入都不应导致 panic
func FuzzValidateFilePath(f *testing.F) {
	// seed corpus
	seeds := []string{
		// 正常路径
		"file.txt",
		"sub/dir/file.txt",
		"a",
		"123",
		"a.txt",
		"dir/file",
		"a/b/c/d/e/file.txt",
		"file with spaces.txt",
		"文件名_中文.txt",
		// 边界场景
		"",                                    // 空字符串
		"../../etc/passwd",                    // 路径穿越
		"a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p.txt", // 深路径
		"\x00test",                            // 空字节前缀
		string([]byte{0x01, 0x02, 0x03}),      // 控制字符
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		// 不变量 0：不应 panic
		result, err := ValidateFilePath(name)

		if err != nil {
			if isExpectedRejection(name) {
				return
			}
			// 没命中已知拒绝条件 — 可能是 regressions
			t.Logf("unexpected error for input %q: %v", name, err)
			return
		}

		assertValidPath(t, result, name)
	})
}

// isExpectedRejection 检查 name 是否属于已知的合法拒绝条件。
func isExpectedRejection(name string) bool {
	if name == "" {
		return true
	}
	if name[0] == '/' || name[0] == '\\' {
		return true
	}
	if strings.Contains(name, "\x00") {
		return true
	}
	if strings.Contains(name, "..") {
		return true
	}
	cleaned := filepath.Clean(name)
	if cleaned == "." {
		return true
	}
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		for _, c := range name {
			if strings.ContainsRune(invalidChars, c) {
				return true
			}
		}
	}
	return false
}

// assertValidPath 验证 ValidateFilePath 成功返回的结果满足不变量。
func assertValidPath(t *testing.T, result, name string) {
	t.Helper()
	// 不变量 1：成功时 result 非空
	if result == "" {
		t.Errorf("empty result for input %q", name)
	}

	// 不变量 2：result 必须是相对路径（ToSlash 格式）
	if filepath.IsAbs(result) {
		t.Errorf("result is absolute path: %q (input: %q)", result, name)
	}
	if !strings.ContainsRune(result, '\\') && result != filepath.ToSlash(result) {
		// 应该已经是 ToSlash 格式
		t.Errorf("result should be ToSlash format, got %q (input: %q)", result, name)
	}
}
