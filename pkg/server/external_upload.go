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
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// externalUploadSink 是 files.UploadSink 的外部卷实现（包装 sync.FS）。
//
// key 语义（评审 M3 修复，2026-10-05）：**owner 隔离**——本地卷键
// `<root>/<owner>/user/<name>`（owner 在租户根），外部卷无租户根，故 WriteFile 收到
// 域侧 rel（`user/<name>`）后映射为 `<owner>/user/<name>`（owner 前缀），与读路径
// resolveExternalDownload 的 ownerKey 同一约定——键空间闭合 + 跨 owner 隔离。
type externalUploadSink struct {
	fs    syncpkg.FS
	owner string // owner 前缀（normalize 后；隔离键）
}

// ownerKey 映射域侧 rel → 外部卷键：`<owner>/<rel>`。
func (s *externalUploadSink) ownerKey(rel string) string {
	return s.owner + "/" + strings.TrimPrefix(rel, "/")
}

// MakeDir 实现 files.UploadSink：创建目录（含中间目录；后端幂等语义）。
func (s *externalUploadSink) MakeDir(ctx context.Context, path string) error {
	return s.fs.MakeDir(ctx, s.ownerKey(path))
}

// WriteFile 实现 files.UploadSink：整流写入（外部卷无原子写）。
func (s *externalUploadSink) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	return s.fs.WriteFile(ctx, s.ownerKey(path), r, size, mtime)
}

// Stat 实现 files.UploadSink：返回路径大小（覆盖写差分用；不存在 → (0,false,nil)）。
func (s *externalUploadSink) Stat(ctx context.Context, path string) (int64, bool, error) {
	e, err := s.fs.Stat(ctx, s.ownerKey(path))
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
	return s.fs.Delete(ctx, s.ownerKey(path))
}

// 编译期断言：externalUploadSink 实现 files.UploadSink。
var _ files.UploadSink = (*externalUploadSink)(nil)
