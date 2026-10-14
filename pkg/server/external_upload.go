// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// external_upload.go 实现外部卷写入源（2026-10-05 用户裁定：普通上传进外部卷 +
// volume.ResolveUserLocation + FSPath 统一键计算）。把外部卷后端（baidupcs / secretdata 等
// sync.FS）适配为 files.UploadSink 消费者接口——域侧经它写文件（整流，无本地
// inode 原子/去重/版本语义），装配层在此包装。

import (
	"context"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// externalUploadSink 是 files.UploadSink 的外部卷实现（包装 sync.FS）。
//
// key 语义（2026-10-05 统一入口）：域侧 rel（`user/<name>`）剥桶后经
// v.ResolveUserLocation(owner, rel) 解析 + FSPath 拼接最终键——共享卷自动加 `<owner>/`
// 前缀（跨 owner 隔离），独享卷无前缀直接路径存取。使用方不关心卷内键算法；
// 权限（Authorize）+ 路径安全（逃逸/注入）由 ResolveUserLocation 一并保证。
type externalUploadSink struct {
	fs    syncpkg.FS
	v     volume.Volume
	owner string
}

// ownerKey 映射域侧 rel → 外部卷键（ResolveUserLocation 解析 + FSPath 拼接；
// 域侧 rel 形如 user/<name>，剥桶前缀后传 user 桶相对路径。**基于 locator 操作**——
// 键拼接只在 FSPath，调用方不自行改路径）。桶目录自身（"user"）剥桶前缀为空 → 键 = user
// 桶自身（2026-10-06 修复：TrimPrefix("user","user/") 不命中会把 "user" 当桶内路径，产生
// "user/user" 残留空目录）。
//
// **嵌套封装卷例外（过渡层）**：嵌套 target（`<卷>/<新子目录>`，SubFS 视图到底层卷子目录）
// 的键空间 = 租户根 rel **原样**（`user/<name>`）——SubFS 的 subdir 前缀之下直接是 user 桶，
// owner 已由卷层隔离（SubFS 工厂按 owner 定向到底层卷）。若按 ResolveUserLocation 恒加
// `<owner>/` 前缀，会在子目录内多一层 owner 目录（`videos/alice/user/…`），数据错位。
func (s *externalUploadSink) ownerKey(rel string) (string, error) {
	if target, _ := s.v.Extra["target"].(string); target != "" {
		if _, _, nested := volume.SplitNestedTarget(target); nested {
			return rel, nil
		}
	}
	stripped := rel
	switch {
	case rel == "user":
		stripped = ""
	case strings.HasPrefix(rel, "user/"):
		stripped = strings.TrimPrefix(rel, "user/")
	}
	loc, err := s.v.ResolveUserLocation(s.owner, stripped)
	if err != nil {
		return "", err
	}
	return loc.FSPath(), nil
}

// MakeDir 实现 files.UploadSink：创建目录（含中间目录；后端幂等语义）。
func (s *externalUploadSink) MakeDir(ctx context.Context, path string) error {
	key, err := s.ownerKey(path)
	if err != nil {
		return err
	}
	return s.fs.MakeDir(ctx, key)
}

// WriteFile 实现 files.UploadSink：整流写入（外部卷无原子写）。
func (s *externalUploadSink) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	key, err := s.ownerKey(path)
	if err != nil {
		return err
	}
	return s.fs.WriteFile(ctx, key, r, size, mtime)
}

// Stat 实现 files.UploadSink：返回路径大小（覆盖写差分用；不存在 → (0,false,nil)）。
func (s *externalUploadSink) Stat(ctx context.Context, path string) (int64, bool, error) {
	key, err := s.ownerKey(path)
	if err != nil {
		return 0, false, err
	}
	e, serr := s.fs.Stat(ctx, key)
	if serr != nil || e == nil {
		if serr != nil {
			return 0, false, serr
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
	key, err := s.ownerKey(path)
	if err != nil {
		return err
	}
	return s.fs.Delete(ctx, key)
}

// 编译期断言：externalUploadSink 实现 files.UploadSink。
var _ files.UploadSink = (*externalUploadSink)(nil)
