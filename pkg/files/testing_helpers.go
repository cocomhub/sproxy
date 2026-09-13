// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"log/slog"
	"time"
)

// testing_helpers.go 集中**仅供测试**使用的导出 helper。
//
// 它们必须留在非 *_test.go 文件里：Go 的 *_test.go 符号无法被其他包导入，而 pkg/server 的
// 测试同样要构造 *UploadStore。文件内的注释显式标注测试专用，避免被误当作生产入口。

// MustNewUploadStore 创建 UploadStore，失败时 panic。
//
// 仅供跨包测试使用（pkg/files 与 pkg/server），因此必须导出；生产路径请用
// NewUploadStore 并自行处理 error。
func MustNewUploadStore(baseDir string, sessionTTL time.Duration, logger *slog.Logger, volumeRoots ...map[string]string) *UploadStore {
	us, err := NewUploadStore(baseDir, sessionTTL, logger, volumeRoots...)
	if err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("创建 UploadStore 失败", "error", err)
		panic("创建 UploadStore 失败: " + err.Error())
	}
	return us
}
