// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_api.go 实现多卷深度 API（任务 6c / 规格 §10、§7 move）：
//   - GET /api/volumes：owner 可见卷列表（ACL per-owner，绝不可全局列卷），usage = 卷容量池 Usage。
//   - POST /api/volumes/move?from_volume&to_volume&filename：跨卷移动同 owner 同 rel。
//
// move 语义（AD-4 唯一 / AD-6 ACL / AD-7 双账本）：
//   - from/to 都须在 owner 卷视图内（否则 403）；源在 from 卷（否则 404）；目标唯一性查重
//     （视图内其它卷（含 to）已有同 rel → 409，防跨卷双份）；from==to 同卷直接无操作成功；
//   - 跨卷 = to 侧双 reserve（owner 全局 + 目标卷池，先 reserve 防中间超限）→ 流式复制到
//     to 卷临时文件 → fsync → 原子 rename → 删源 → 双 commit（to）→ 双 release（from）；
//   - 并发/幂等：同 rel 移动与上传共用 uploadingFiles 锁（同文件并发 move 恰一成功，其余 409）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// VolumeStatus 是 GET /api/volumes 中单个卷的描述（仅 owner 允许卷；allowed 恒 true——
// 列表本身只含允许卷，字段保留给客户端/未来扩展）。
type VolumeStatus struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`     // 卷 ACL 模式（allow|deny）
	Capacity int64  `json:"capacity"` // 卷容量上限（0 = 不限）
	Usage    int64  `json:"usage"`    // 卷容量池当前已用字节
	Allowed  bool   `json:"allowed"`  // owner 是否允许使用（列表内恒 true）
}

type volumesListResponse struct {
	Volumes []VolumeStatus `json:"volumes"`
}

// listVolumesHandler 处理 GET /api/volumes。返回 owner 卷视图（AllowedVolumes）内每卷的
// name/mode/capacity/usage/allowed；usage 取该卷容量池 Usage（0 = 池未装配/旧路径）。
// per-owner 安全：以 owner 视图为界，ACL 收紧卷绝不列出（AD-6 可见性）。volSet nil（旧装配
// 路径，无卷语义）返回空列表 200。
func (h *Handlers) listVolumesHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	if h.volSet == nil {
		sendJSONResponse(w, volumesListResponse{Volumes: []VolumeStatus{}}, http.StatusOK)
		return
	}
	view := volume.AllowedVolumes(h.volSet.All(), owner)
	out := make([]VolumeStatus, 0, len(view))
	for _, v := range view {
		var usage int64
		if p := h.volSet.Pool(v.Name); p != nil {
			usage = p.Usage()
		}
		out = append(out, VolumeStatus{
			Name:     v.Name,
			Mode:     string(v.ACL.Mode),
			Capacity: v.Capacity,
			Usage:    usage,
			Allowed:  true,
		})
	}
	sendJSONResponse(w, volumesListResponse{Volumes: out}, http.StatusOK)
}

// removeMovedSource 是 move 删源的可替换测试 seam：默认直接委托 storage.Root.Remove。
// 测试可临时替换以确定性模拟「并发 delete 在 move stat 与 Remove 之间已删源」的 IsNotExist
// 竞态（真实并发时序跨平台不可确定——Windows 上源文件在 move 复制期间被打开，并发 Remove
// 通常共享冲突失败而非 IsNotExist）。生产路径不替换。
var removeMovedSource = func(root *storage.Root, rel string) error {
	return root.Remove(rel)
}

// moveVolumeHandler 处理 POST /api/volumes/move（参数与语义见文件头注释）。
func (h *Handlers) moveVolumeHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	fromVol := r.URL.Query().Get("from_volume")
	toVol := r.URL.Query().Get("to_volume")
	if filename == "" || fromVol == "" || toVol == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "from_volume、to_volume、filename 均不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}
	if h.volSet == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "卷功能未装配，无法跨卷移动"}, http.StatusBadRequest)
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// ACL 视图（AD-6/§8）：from/to 都必须在 owner 视图内，否则 403（不泄卷存在性）。
	fv, fOK := h.volSet.ByName(fromVol)
	if !fOK || !fv.Authorize(owner) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "volume not allowed"}, http.StatusForbidden)
		return
	}
	tv, tOK := h.volSet.ByName(toVol)
	if !tOK || !tv.Authorize(owner) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "volume not allowed"}, http.StatusForbidden)
		return
	}

	// 并发同文件防护：同 owner+rel 移动/上传共用 uploadingFiles 锁池（与 upload handler 同键），
	// 串行化并发 move / move×upload；后到者 409。锁在参数/ACL 校验之后、源校验之前获取，
	// 使「源仍存在」「目标唯一」判定在锁内完成（accept 先后校验，防竞态迁走/迁入）。
	upKey := owner + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, uploadingLockMove); loaded {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中"}, http.StatusConflict)
		return
	}
	defer h.uploadingFiles.Delete(upKey)

	// 源存在性（锁内判定）。fromTnt 取 from 卷上 owner 租户（写盘 root）。
	fromTnt := h.volumeTenant(fromVol, owner)
	if fromTnt == nil || fromTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	fromRoot := fromTnt.Root()
	srcInfo, statErr := fromRoot.Stat(rel)
	if os.IsNotExist(statErr) {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "源文件不存在",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	} else if statErr != nil {
		h.logger.Error("move: stat 源文件失败", "file_name", remotePath, "volume", fromVol, "error", statErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问源文件失败"}, http.StatusInternalServerError)
		return
	}
	if srcInfo.IsDir() {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "不能移动目录（仅支持文件）"}, http.StatusBadRequest)
		return
	}

	// 同卷 move：源已在目标卷（rel 不变即目标位置就是源自身）→ 无操作成功（幂等）。
	if fromVol == toVol {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess, Detail: "已在目标卷（同卷 no-op）",
		})
		sendJSONResponse(w, UploadResponse{Success: true, Message: "文件已在目标卷，无需移动"}, http.StatusOK)
		return
	}

	// 目标唯一性（AD-4）：视图内其它卷（含 to）已存在同 rel → 409（防跨卷双份 / 静默覆盖）。
	conflictVol, cerr := h.checkMoveTargetUnique(owner, rel, fromVol, toVol)
	if cerr != nil {
		h.logger.Error("move: 目标唯一性探测失败", "file_name", remotePath, "error", cerr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "探测目标卷失败"}, http.StatusInternalServerError)
		return
	}
	if conflictVol != "" {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "目标卷已存在同名: " + conflictVol,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "目标卷已存在同名文件"}, http.StatusConflict)
		return
	}

	// 目标卷租户与容量池（写盘 root）。
	toTnt := h.volumeTenant(toVol, owner)
	if toTnt == nil || toTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	toPool := h.volSet.Pool(toVol)

	// AD-7 双账本 to 侧先 reserve（owner 全局 → 目标卷池，防中间超限）；任一失败回滚已预留。
	size := srcInfo.Size()
	scope := h.quotaScopeFor(owner, rel)
	var scopeRes, poolRes *quota.Reservation
	if scope != nil {
		rr, rerr := scope.TryReserve(size)
		if rerr != nil {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
		}
		scopeRes = rr
	}
	if toPool != nil {
		rr, rerr := toPool.TryReserve(size)
		if rerr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
		}
		poolRes = rr
	}

	// 流式复制到 to 卷（临时 + fsync + 原子 rename）；失败回滚双预留，源不动。
	written, copyErr := crossVolumeCopy(r.Context(), fromRoot, toTnt.Root(), rel, rel)
	if copyErr != nil {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "复制到目标卷失败",
		})
		h.logger.Error("move: 复制到目标卷失败", "file_name", remotePath, "from", fromVol, "to", toVol, "error", copyErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "移动文件失败"}, http.StatusInternalServerError)
		return
	}
	// 纵深防御（TOCTOU 闭合）：复制字节数须与 stat 源尺寸一致——不一致 = 源在复制中被并发改写/
	// 截断（uploadingFiles 锁已挡住同 rel upload/move，delete/restore 未持锁）。fail-closed：
	// 删目标 + 双 Release 回滚，源不动，不把「不确定内容」当移动成功提交。
	if written != size {
		_ = toTnt.Root().Remove(rel)
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "复制字节与源尺寸不一致（源被并发改写），已回滚",
		})
		h.logger.Error("move: 复制字节与源尺寸不一致，已回滚", "file_name", remotePath,
			"from", fromVol, "to", toVol, "size", size, "written", written)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "移动文件失败"}, http.StatusInternalServerError)
		return
	}

	// 删源三分支：
	//   - 成功（nil）→ 双 commit（to 侧预留对账为实际占用 written）+ from 侧释放（owner 全局
	//     + from 卷池）——本函数正常移动路径；
	//   - IsNotExist = 并发 delete（不持 uploadingFiles 锁）已先删源并**已释放 from 侧账本**
	//     （目标卷已持数据）→ **只 commit to 侧**、立即 return，绝不 from 侧再释放（否则 owner
	//     全局 + from 卷池各欠计 S，PR-D F1）；回包按成功（源消失但数据已落目标卷，移动语义完成）；
	//   - 其它错误 → 回滚 to 侧（删目标 + 释放预留），源保留。
	rmErr := removeMovedSource(fromRoot, rel)
	if rmErr != nil {
		if errors.Is(rmErr, os.ErrNotExist) {
			if scopeRes != nil {
				scopeRes.Commit(written)
			}
			if poolRes != nil {
				poolRes.Commit(written)
			}
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "volume_move", ObjectType: "file", Object: remotePath,
				Result: AuditResultSuccess, Detail: "源已被并发删除，目标卷已持数据（from 侧账本由并发 delete 释放）",
			})
			h.logger.Info("跨卷移动完成：源已被并发删除", "file_name", remotePath, "from", fromVol, "to", toVol)
			sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已移动: %s (%s → %s)", remotePath, fromVol, toVol)}, http.StatusOK)
			return
		}
		_ = toTnt.Root().Remove(rel)
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "删除源文件失败（已回滚目标）",
		})
		h.logger.Error("move: 删除源文件失败（已回滚）", "file_name", remotePath, "error", rmErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "移动文件失败"}, http.StatusInternalServerError)
		return
	}

	// 双 commit（to 侧预留对账为实际占用 written）+ from 侧释放（owner 全局 + from 卷池）。
	if scopeRes != nil {
		scopeRes.Commit(written)
	}
	if poolRes != nil {
		poolRes.Commit(written)
	}
	if scope != nil {
		scope.ReleaseUsage(written)
	}
	if fromPool := h.volSet.Pool(fromVol); fromPool != nil {
		fromPool.ReleaseCommitted(written)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "volume_move", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "from=" + fromVol + " to=" + toVol,
	})
	h.logger.Info("跨卷移动成功", "file_name", remotePath, "from", fromVol, "to", toVol)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已移动: %s (%s → %s)", remotePath, fromVol, toVol)}, http.StatusOK)
}

// checkMoveTargetUnique 检查把 rel 从 fromVol 迁到 toVol 是否破坏 AD-4 唯一性（owner 视图内
// 除源卷外任何卷已存在同 rel，含目标卷 toVol）。返回冲突所在卷名（空 = 无冲突）与错误
// （stat 探测非「不存在」错误 → 500，fail-closed）。fromVol != toVol 由调用方保证。
func (h *Handlers) checkMoveTargetUnique(owner, rel, fromVol, toVol string) (string, error) {
	view := volume.AllowedVolumes(h.volSet.All(), owner)
	for _, v := range view {
		if v.Name == fromVol {
			continue // 源文件自身所在卷不算冲突
		}
		exists, err := h.volumeFileExists(v.Name, owner, rel)
		if err != nil {
			return "", err
		}
		if exists {
			return v.Name, nil
		}
	}
	return "", nil
}

// crossVolumeCopy 把 srcRoot 上 srcRel 文件流式复制到 dstRoot 上 dstRel（跨卷 move/恢复用）。
// 全程 root 相对：目标目录自动 MkdirAll，先写同目录唯一临时文件（O_EXCL）、fsync、Close 后
// 原子 rename（atomicRenameRoot，Windows 并发退避）。返回复制字节数（供配额 Commit/release）。
// 失败路径幂等清理临时文件；成功路径临时文件已 rename 不存在，defer Remove 为空操作。
func crossVolumeCopy(ctx context.Context, srcRoot, dstRoot *storage.Root, srcRel, dstRel string) (int64, error) {
	src, err := srcRoot.Open(srcRel)
	if err != nil {
		return 0, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	dir := filepath.Dir(dstRel)
	if err = dstRoot.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("创建目标目录失败: %w", err)
	}
	tmpRel := filepath.Join(dir, filepath.Base(dstRel)+fmt.Sprintf(".tmp.%d", time.Now().UnixNano()))
	dst, err := dstRoot.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("创建目标临时文件失败: %w", err)
	}
	defer func() { _ = dstRoot.Remove(tmpRel) }()

	written, err := copyWithContext(dst, src, ctx)
	if err != nil {
		_ = dst.Close()
		return 0, fmt.Errorf("复制文件失败: %w", err)
	}
	if err = dst.Sync(); err != nil {
		_ = dst.Close()
		return 0, fmt.Errorf("同步目标文件失败: %w", err)
	}
	if err = dst.Close(); err != nil {
		return 0, fmt.Errorf("关闭目标文件失败: %w", err)
	}
	if err = atomicRenameRoot(dstRoot, tmpRel, dstRel); err != nil {
		return 0, fmt.Errorf("原子重命名目标文件失败: %w", err)
	}
	return written, nil
}
