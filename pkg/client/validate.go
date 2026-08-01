// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"os"
	"path/filepath"
	"strings"
)

// containsPathTraversal 检查路径是否包含语义上的路径穿越。
// filepath.Clean 已解析语义上的 ..，但 foo/../../bar 仍可能得出 ../bar，
// 需要额外检查清理后路径是否以 .. 开头或包含 /../ 段。
func containsPathTraversal(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(path))
	return cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../")
}

// ensureParentDir 确保输出路径的父目录存在，不存在则创建。
func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}
