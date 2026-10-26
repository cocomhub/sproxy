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
//   - extra 走 pkg/files/meta 白名单校验（fail-closed）；
//   - 底层 trusted_fs.UpdateMetaExtra 做前置读回校验（并发覆盖时拒绝），我们只透传。

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/volume"
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
	// 双路定位（2026-10-26）：本地卷经 locateForRead（*storage.Root）；外部卷
	// （secretdata/baidupcs/s3 无本地 root）经 ExternalMetaUpdate 接缝。
	loc, ok := s.locateForRead(owner, mustRel(s, owner, input.RemotePath), "")
	if ok && loc.Tenant != nil {
		return s.updateMetaLocal(ctx, owner, input.RemotePath, loc, input.Extra)
	}
	return s.updateMetaExternal(ctx, owner, input.RemotePath, input.Extra)
}

// updateMetaLocal 本地卷分支：经 FileMetaPolicy 的读-校验-合并-写（filesMetaPolicy
// 读 root 相对 sidecar + TrustedVolumeFS.UpdateMetaExtra 合并）。
func (s *Service) updateMetaLocal(ctx context.Context, owner, remotePath string, loc FileLocation, extra map[string]any) error {
	updater := s.metaExtraUpdaterFor(loc)
	if updater == nil {
		return ErrMetaUnavailable
	}
	if err := updater.UpdateMetaExtra(ctx, remotePath, extra); err != nil {
		return fmt.Errorf("files: 更新 meta extra 失败: %w", err)
	}
	return nil
}

// updateMetaExternal 外部卷分支：经 ExternalVolumeOps.ExternalMetaUpdate 接缝
// （装配层经同一可信链 Guard(Wrap(...)) 执行 UpdateMetaExtra）。外部卷未命中
// （卷不存在/未授权/文件不存在）→ 404 语义（与 locateForRead 未命中一致）。
func (s *Service) updateMetaExternal(ctx context.Context, owner, remotePath string, extra map[string]any) error {
	ops := s.rt.externalVolOps()
	if ops == nil {
		return &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	vs := s.rt.volSet()
	if vs == nil {
		return &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	// 按 owner 可见卷顺序探测：命中第一个「文件存在且可更新」的外部卷（与
	// copyWithinVolume/moveUniqueConflict 的 AllowedVolumes 双路模型同构）。
	for _, v := range volume.AllowedVolumes(vs.All(), owner) {
		if vs.HasLocalRoot(v.Name) {
			continue // 本地卷已在上方 locateForRead 处理
		}
		hit, herr := ops.ExternalMetaUpdate(ctx, owner, v.Name, remotePath, extra)
		if herr != nil {
			return herr
		}
		if hit {
			return nil
		}
	}
	return &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
}

// GetMetaInput 是元数据读入参。
type GetMetaInput struct {
	// Owner 是操作主体（空 → anonymous）。
	Owner string
	// RemotePath 是用户可见相对路径。
	RemotePath string
}

// GetMeta 读取目标文件的自定义元数据 extra（双路定位：本地卷读 meta 桶 sidecar /
// 外部卷经 ExternalMeta 接缝）。返回 nil = 文件不存在或无 extra（读侧零校验语义）。
func (s *Service) GetMeta(ctx context.Context, input GetMetaInput) (map[string]any, error) {
	owner := normalizeOwner(input.Owner)
	loc, ok := s.locateForRead(owner, mustRel(s, owner, input.RemotePath), "")
	if ok && loc.Tenant != nil {
		return s.getMetaLocal(ctx, owner, input.RemotePath, loc)
	}
	return s.getMetaExternal(ctx, owner, input.RemotePath)
}

// getMetaLocal 本地卷分支：读 `meta/<rel>.meta` sidecar → FileMeta.Extra（深拷贝）。
// sidecar 缺失 / 无 extra → 空 map（读侧零校验语义，与 VerifyDownload 的缺失直通一致）。
func (s *Service) getMetaLocal(ctx context.Context, owner, remotePath string, loc FileLocation) (map[string]any, error) {
	tnt := loc.Tenant
	if tnt == nil || tnt.Root() == nil {
		return nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return nil, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	mrel := meta.MetaPath(rel)
	raw, rerr := s.rt.readMetaSidecar(ctx, tnt.Root(), mrel)
	if errors.Is(rerr, os.ErrNotExist) {
		return map[string]any{}, nil // sidecar 缺失 = 无 extra（不报错）
	}
	if rerr != nil {
		return nil, rerr // 存在但不可读 → fail-closed
	}
	fm, umerr := meta.Unmarshal(raw)
	if umerr != nil {
		return nil, fmt.Errorf("files: sidecar 损坏（拒绝返回）: %w", umerr)
	}
	out := make(map[string]any, len(fm.Extra))
	maps.Copy(out, fm.Extra)
	return out, nil
}

// getMetaExternal 外部卷分支：经 ExternalVolumeOps.ExternalMeta 接缝。未命中 → 404。
func (s *Service) getMetaExternal(ctx context.Context, owner, remotePath string) (map[string]any, error) {
	ops := s.rt.externalVolOps()
	if ops == nil {
		return nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	vs := s.rt.volSet()
	if vs == nil {
		return nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	for _, v := range volume.AllowedVolumes(vs.All(), owner) {
		if vs.HasLocalRoot(v.Name) {
			continue
		}
		extra, ok2, err := ops.ExternalMeta(ctx, owner, v.Name, remotePath)
		if err != nil {
			return nil, err
		}
		if ok2 {
			return extra, nil
		}
	}
	return nil, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
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
