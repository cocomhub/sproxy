// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// meta_update.go 实现「已上传文件的元数据补录/修正」（方案②：PUT /api/files/meta）。
//
// 场景：文件已上传但当时未带 meta（或需修正 maker/genres 等）。走**旁路 Extra 更新**
// （pkg/sync.MetaExtraUpdater）：读-校验-合并-写 sidecar，不重算内容哈希（只信任既有
// meta 与主文件一致）。
//
// 安全（数据安全完整可靠）：
//   - 文件必须已存在且已有可用 sidecar（无 meta = 无内容基线，拒绝盲写——避免把
//     「未知内容」标成可信）；
//   - extra 走 pkg/files/metaextra 白名单校验（fail-closed）；
//   - 底层 trusted_fs.UpdateMetaExtra 做前置读回校验（并发覆盖时拒绝），我们只透传。

import (
	"context"
	"errors"
	"fmt"
)

// ErrMetaUnavailable 表示目标未装配旁路 meta 更新能力（如外部卷 / 未启用可信卷）。
var ErrMetaUnavailable = errors.New("files: 目标卷未装配 meta 旁路更新能力")

// UpdateMetaInput 是元数据补录入参。
type UpdateMetaInput struct {
	// Owner 是操作主体（空 → anonymous）。
	Owner string
	// RemotePath 是用户可见相对路径。
	RemotePath string
	// Extra 是要合并的自定义元数据（白名单校验由调用方或本方法内执行）。
	Extra map[string]any
}

// UpdateMeta 把 Extra 合并进目标文件的 .meta sidecar（方案②）。
//
// 前置：文件须存在且已有 sidecar（内容基线）；否则返回 ErrMetaUnavailable /
// 底层校验错误（fail-closed，不盲写）。
func (s *Service) UpdateMeta(ctx context.Context, input UpdateMetaInput) error {
	owner := normalizeOwner(input.Owner)
	if len(input.Extra) == 0 {
		return nil
	}
	loc, ok := s.locateForRead(owner, mustRel(s, owner, input.RemotePath), "")
	if !ok || loc.Tenant == nil {
		return fmt.Errorf("files: 目标文件不存在或不可定位: %s", input.RemotePath)
	}
	updater := s.metaExtraUpdaterFor(loc)
	if updater == nil {
		return ErrMetaUnavailable
	}
	if err := updater.UpdateMetaExtra(ctx, input.RemotePath, input.Extra); err != nil {
		return fmt.Errorf("files: 更新 meta extra 失败: %w", err)
	}
	return nil
}

// metaExtraUpdaterFor 取目标卷 FS 的旁路 Extra 能力（未装配 → nil）。
// 走 VolumeSet 的 metaExtraUpdater 能力（装配层注入；与 UpdateMetaExtra 装饰器链一致）。
func (s *Service) metaExtraUpdaterFor(loc FileLocation) MetaExtraUpdater {
	vs := s.rt.volSet()
	if vs == nil {
		return nil
	}
	u, ok := vs.(metaExtraUpdaterSource)
	if !ok {
		return nil
	}
	return u.MetaExtraUpdater(loc.VolumeName, "")
}

// MetaExtraUpdater 是旁路 Extra 更新的最小接口（装配层经 metaExtraUpdaterSource 注入）。
type MetaExtraUpdater interface {
	UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
}

// metaExtraUpdaterSource 由装配层卷集合实现（提供按卷取旁路更新能力）。
type metaExtraUpdaterSource interface {
	MetaExtraUpdater(volName, owner string) MetaExtraUpdater
}

// mustRel 把用户可见路径解析为根内相对路径（owner/… 形态）。失败返回原路径（
// 调用方 locateForRead 会再判失败）。
func mustRel(s *Service, owner, remotePath string) string {
	_, rel, err := s.resolveWritePath(owner, remotePath)
	if err != nil {
		return remotePath
	}
	return rel
}

// RecordFileAudit 是 runtime.recordFileAudit 的导出入口（装配层 handler 记录业务审计用）。
// action 为业务动作（如 file_meta_update），object 为目标路径，result 为 success/failed。
func (s *Service) RecordFileAudit(ctx context.Context, action, object, result, detail string) {
	s.rt.recordFileAudit(ctx, action, object, result, detail)
}
