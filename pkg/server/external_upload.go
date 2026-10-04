// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// external_upload.go 实现外部卷写入源（2026-10-05 用户裁定：普通上传进外部卷 +
// 统一 user/ 前缀）。把外部卷后端（baidupcs / secretdata 等 sync.FS）适配为
// files.UploadSink 消费者接口——域侧经它写文件（整流，无本地 inode 原子/去重/版本
// 语义），装配层在此包装。

import (
	"context"
	"io"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// externalUploadSink 是 files.UploadSink 的外部卷实现（包装 sync.FS）。
// key 语义：直接透传 rel（统一 user/<rel>，与读路径 resolveExternalDownload 一致——
// 普通上传落 user/<rel>，/download 经 UserRel 也产出 user/<rel>，键空间闭合）。
type externalUploadSink struct {
	fs syncpkg.FS
}

// MakeDir 实现 files.UploadSink：创建目录（含中间目录；后端幂等语义）。
func (s *externalUploadSink) MakeDir(ctx context.Context, path string) error {
	return s.fs.MakeDir(ctx, path)
}

// WriteFile 实现 files.UploadSink：整流写入（外部卷无原子写）。
func (s *externalUploadSink) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	return s.fs.WriteFile(ctx, path, r, size, mtime)
}

// Stat 实现 files.UploadSink：返回路径大小（覆盖写差分用；不存在 → (0,false,nil)）。
func (s *externalUploadSink) Stat(ctx context.Context, path string) (int64, bool, error) {
	e, err := s.fs.Stat(ctx, path)
	if err != nil || e == nil {
		if err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	if e.IsDir {
		return 0, true, nil
	}
	return e.Size, true, nil
}

// Remove 实现 files.UploadSink：删除路径（checksum 校验失败清理用）。
func (s *externalUploadSink) Remove(ctx context.Context, path string) error {
	return s.fs.Delete(ctx, path)
}

// 编译期断言：externalUploadSink 实现 files.UploadSink。
var _ files.UploadSink = (*externalUploadSink)(nil)
