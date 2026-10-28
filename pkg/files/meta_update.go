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
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// ErrMetaUnavailable 表示目标未装配旁路 meta 更新能力（如外部卷 / 未启用可信卷）。
var ErrMetaUnavailable = errors.New("files: 目标卷未装配 meta 旁路更新能力")

// MetaUpdateMode 是元数据补录的合并模式（用户裁定 2026-10-26：默认**新增而非覆盖**）。
type MetaUpdateMode string

const (
	// MetaUpdateMerge 把 extra 合并进现有（同名键更新，其余保留）——**默认**。
	MetaUpdateMerge MetaUpdateMode = "merge"
	// MetaUpdateReplace 整表替换（丢弃所有旧键，只保留本次 extra）——显式覆盖。
	MetaUpdateReplace MetaUpdateMode = "replace"
	// MetaUpdateDelete 删除指定键（Extra 的 key 集合为待删键，value 忽略）。
	MetaUpdateDelete MetaUpdateMode = "delete"
)

// UpdateMetaInput 是元数据补录入参。
type UpdateMetaInput struct {
	// Owner 是操作主体（空 → anonymous）。
	Owner string
	// RemotePath 是用户可见相对路径。
	RemotePath string
	// Extra 是要写入的自定义元数据（白名单校验由调用方或本方法内执行）。
	// MetaUpdateDelete 时只取 key 集合（value 忽略）。
	Extra map[string]any
	// Mode 是合并模式（零值 = MetaUpdateMerge，保持既有语义）。
	Mode MetaUpdateMode
}

// UpdateMeta 把 Extra 合并进目标文件的 .meta sidecar（方案②）。
//
// 前置：文件须存在且已有 sidecar（内容基线）；否则返回 ErrMetaUnavailable /
// 底层校验错误（fail-closed，不盲写）。
func (s *Service) UpdateMeta(ctx context.Context, input UpdateMetaInput) error {
	owner := normalizeOwner(input.Owner)
	if len(input.Extra) == 0 && input.Mode != filesMetaDeleteMode() {
		return nil
	}
	// 更新模式经 ctx 承载（同步到 syncpkg.MetaUpdateMode），供底层 UpdateMetaExtra 分支。
	ctx = syncpkg.WithMetaUpdateMode(ctx, syncModeOf(input.Mode))
	// 双路定位（2026-10-26）：本地卷经 locateForRead（*storage.Root）；外部卷
	// （secretdata/baidupcs/s3 无本地 root）经 ExternalMetaUpdate 接缝。
	loc, ok := s.locateForRead(owner, mustRel(s, owner, input.RemotePath), "")
	if ok && loc.Tenant != nil {
		return s.updateMetaLocal(ctx, owner, input.RemotePath, loc, input.Extra)
	}
	return s.updateMetaExternal(ctx, owner, input.RemotePath, input.Extra)
}

// updateMetaLocal 本地卷分支（2026-10-26 修复 P0）：委派装配层 FileMetaPolicy.UpdateMetaExtra
// ——**不再**走 MetaExtraUpdater 接缝。
//
// 为什么：MetaExtraUpdater 接缝按卷名查 Set.External(volName)，而**本地卷不在 external**
// （assembleLocalVolume 只写 roots）⇒ 恒 nil ⇒ 本地卷 meta set 恒 503「未装配 meta 旁路
// 更新能力」。本地卷 sidecar 本就由 FileMetaPolicy 维护 ⇒ 直接委派。
func (s *Service) updateMetaLocal(ctx context.Context, owner, remotePath string, loc FileLocation, extra map[string]any) error {
	if loc.Tenant == nil || loc.Tenant.Root() == nil {
		return &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	rel, ok := loc.Tenant.UserRel(remotePath)
	if !ok {
		return &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return s.rt.updateMetaExtra(ctx, owner, loc.Tenant.Root(), rel, extra)
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

// MetaInfo 是 meta 读取的完整结果（extra + 原始文件 mtime/ctime）。
// MTime/CTime 为 RFC3339Nano（UTC；空 = 未知/该卷不提供时间信息）。
type MetaInfo struct {
	Extra map[string]any
	MTime string
	CTime string
}

// GetMetaInfo 读取 meta extra 及原始文件时间（mtime/ctime）。
// 本地卷从 sidecar 的 FileMeta 取（写入时已记录）；外部卷仅返回 extra（时间字段为空）。
func (s *Service) GetMetaInfo(ctx context.Context, input GetMetaInput) (MetaInfo, error) {
	owner := normalizeOwner(input.Owner)
	loc, ok := s.locateForRead(owner, mustRel(s, owner, input.RemotePath), "")
	if ok && loc.Tenant != nil {
		return s.getMetaInfoLocal(ctx, owner, input.RemotePath, loc)
	}
	extra, err := s.getMetaExternal(ctx, owner, input.RemotePath)
	if err != nil {
		return MetaInfo{}, err
	}
	return MetaInfo{Extra: extra}, nil
}

// getMetaInfoLocal 本地卷：sidecar → (extra, mtime, ctime)。
func (s *Service) getMetaInfoLocal(ctx context.Context, owner, remotePath string, loc FileLocation) (MetaInfo, error) {
	tnt := loc.Tenant
	if tnt == nil || tnt.Root() == nil {
		return MetaInfo{}, &HTTPError{Status: http.StatusNotFound, Message: errMsgFileNotFound}
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return MetaInfo{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	raw, rerr := s.rt.readMetaSidecar(ctx, tnt.Root(), meta.MetaPath(rel))
	if errors.Is(rerr, os.ErrNotExist) {
		return MetaInfo{Extra: map[string]any{}}, nil // sidecar 缺失 = 无 extra（不报错）
	}
	if rerr != nil {
		return MetaInfo{}, rerr
	}
	fm, umerr := meta.Unmarshal(raw)
	if umerr != nil {
		return MetaInfo{}, fmt.Errorf("files: sidecar 损坏（拒绝返回）: %w", umerr)
	}
	out := make(map[string]any, len(fm.Extra))
	maps.Copy(out, fm.Extra)
	return MetaInfo{Extra: out, MTime: fm.MTime, CTime: fm.CTime}, nil
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

// filesMetaDeleteMode 返回删除模式的哨兵（空 extra 但 mode=delete 也应执行删键）。
func filesMetaDeleteMode() MetaUpdateMode { return MetaUpdateDelete }

// syncModeOf 把 files 域模式映射到 syncpkg.MetaUpdateMode（单一映射，避免漂移）。
func syncModeOf(m MetaUpdateMode) syncpkg.MetaUpdateMode {
	switch m {
	case MetaUpdateReplace:
		return syncpkg.MetaUpdateReplace
	case MetaUpdateDelete:
		return syncpkg.MetaUpdateDelete
	default:
		return syncpkg.MetaUpdateMerge
	}
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
