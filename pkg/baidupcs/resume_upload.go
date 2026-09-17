// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"fmt"
	"os"

	"github.com/qjfoidnh/BaiduPCS-Go/requester/uploader"
)

// openLocalFile 打开本地文件（上传源），统一错误包装。
func openLocalFile(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("baidupcs: open local %q: %w", path, err)
	}
	return f, nil
}

// globalLayout 是断点持久化的默认布局（Upload 流程使用）。
// 由 NewLibraryAdapter 注入；nil 时断点不持久化（仅内存恢复）。
var globalLayout *Layout

// loadUploadResume 从布局 Resume 目录读取上传断点。
func loadUploadResume(key string) (*uploader.InstanceState, error) {
	if globalLayout == nil {
		return nil, fmt.Errorf("baidupcs: layout not configured")
	}
	var st uploader.InstanceState
	if err := globalLayout.LoadResume(key, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// saveUploadResume 原子写上传断点到布局 Resume 目录。
// 上传完成后调用（成功则断点已含全部 BlockList，下次 Load 时自然跳过已传分片）。
func saveUploadResume(key string, st *uploader.InstanceState) error {
	if globalLayout == nil {
		return nil // 未配置布局：不持久化（调用方容忍）
	}
	if st == nil {
		return nil
	}
	return globalLayout.SaveResume(key, st)
}

// sanitizeRemotePathSize 生成断点 key 的一部分（路径 + 文件大小，文件变更则断点失效）。
func sanitizeRemotePathSize(localPath string) string {
	fi, err := os.Stat(localPath)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", fi.Size())
}
