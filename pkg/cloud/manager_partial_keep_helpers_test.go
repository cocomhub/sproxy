// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"os"
	"path/filepath"
)

// mkdirAllAndWritePartial 在任务目录里造一个 `.partial`（测试辅助）。
func mkdirAllAndWritePartial(dir, filename string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename+".partial"), make([]byte, 2048), 0o644)
}

// fileExists 判断文件是否存在（测试辅助）。
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
