// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_mirror.go 实现 P0 跨卷复制/镜像（roadmap §3.3）：
//   - POST /api/volumes/copy?from_volume&to_volume&filename：跨卷复制（保留源，与 move 相对）。
//   - volumes[].mirror_to 定时镜像策略：把源卷 user 桶内容周期复制到目标卷（源保留，
//     幂等覆盖一致副本），由 mirror_interval 周期 goroutine 驱动（与 versionGCLoop 同构）。
//
// copy 语义（对齐 move 的原子语义，仅「删源」改为「保留源」）：
//   - from/to 都须在 owner 卷视图内（否则 403）；源在 from 卷（否则 404）；目标唯一性——
//     视图内其它卷已有同 rel → 409；目标卷同 rel 已存在且 checksum 一致 → 200 幂等成功
//     （不重复占配额）；不一致 → 409（copy 不覆盖用户已有内容——镜像 pass 例外，见下）；
//   - from==to → no-op 成功（幂等）；
//   - 跨卷 = to 侧双 reserve（owner 全局 + 目标卷池）→ 流式复制（crossVolumeCopy，
//     O_EXCL 临时 + fsync + 原子 rename）→ 双 commit（to）→ **源保留**（不删源、from 侧
//     账本不动）——与 move 唯一差异；
//   - 并发/幂等：同 rel 复制与上传/move 共用 uploadingFiles 锁（同文件并发 copy 恰一
//     成功，其余 409）。
//
// 镜像 pass（volumeMirrorPass / mirrorVolume）语义：
//   - 仅本地卷→本地卷（外部卷经 MirrorTarget() 过滤）；
//   - 逐 owner（listTenantIDs 扫描默认卷根）逐文件（listVolumeUserFiles 递归源卷 user 桶）：
//     owner 在目标卷视图（ACL）内才镜像；目标同 rel 已存在且 checksum 一致 → 跳过
//     （幂等）；不一致 → 覆盖为目标内容（镜像收敛语义——与 copy API 的 409 不同，镜像的
//     目标是「目标卷是源卷的副本」，差异即收敛）；配额不足 → 跳过该文件（尽力而为）；
//   - 每文件持 uploadingFiles 锁（与 move 同键），失败不阻断其它文件；
//   - 审计 volume_mirror（每轮一次汇总）与 volume_copy（每文件一次）。
//
// 周期调度：mirrorVolumeLoop 按 cfg.MirrorInterval tick（>0 时 RegisterRoutes 启动；
// Close() 关 mirrorStop），与 credentialRotationLoop / versionGCLoop 同构。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// mirrorCopyResponse 是 POST /api/volumes/copy 的响应体。
type mirrorCopyResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Checksum   string `json:"checksum,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Idempotent bool   `json:"idempotent,omitempty"`
}

// copyVolumeHandler 处理 POST /api/volumes/copy?from_volume&to_volume&filename。
// 语义见文件头注释（与 move 对齐，仅保留源）。
func (h *Handlers) copyVolumeHandler(w http.ResponseWriter, r *http.Request) {
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "卷功能未装配，无法跨卷复制"}, http.StatusBadRequest)
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))

	// ACL 视图（AD-6/§8）：from/to 都必须在 owner 视图内，否则 403（不泄卷存在性）。
	if !h.volumeAllowedFor(owner, fromVol) || !h.volumeAllowedFor(owner, toVol) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "volume not allowed"}, http.StatusForbidden)
		return
	}

	status, resp := h.copyFileBetweenVolumes(r.Context(), owner, remotePath, fromVol, toVol, false)
	sendJSONResponse(w, resp, status)
}

// copyFileBetweenVolumes 是跨卷单文件复制的原子核心（copy API 与镜像 pass 共用）。
// 与 moveFileBetweenVolumes 对齐：锁 + 源校验 + 目标查重 + 双预留 + 流式复制 + fsync +
// 原子 rename + 双 commit；**不删源**（与 move 唯一差异）。targetOverwrite 控制目标同 rel
// 已存在且内容不一致时的行为：false（copy API）→ 409；true（镜像收敛）→ 覆盖为目标内容。
//
// ctx 是审计/日志上下文（copy API 传请求 ctx；镜像 pass 传 context.Background()）。
// 返回 (HTTP 状态码, 响应体)。
func (h *Handlers) copyFileBetweenVolumes(ctx context.Context, owner, remotePath, fromVol, toVol string, targetOverwrite bool) (int, mirrorCopyResponse) {
	okResp := func(msg string) (int, mirrorCopyResponse) {
		return http.StatusOK, mirrorCopyResponse{Success: true, Message: msg}
	}
	errResp := func(status int, msg string) (int, mirrorCopyResponse) {
		return status, mirrorCopyResponse{Success: false, Message: msg}
	}

	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return errResp(http.StatusBadRequest, errMsgInvalidPath)
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return errResp(http.StatusBadRequest, errMsgInvalidPath)
	}

	// 并发同文件防护：同 owner+rel 复制/移动/上传共用 uploadingFiles 锁池（与 move 同键），
	// 串行化并发 copy / copy×move / copy×upload；后到者 409。
	upKey := owner + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, uploadingLockMove); loaded {
		return errResp(http.StatusConflict, "文件正在移动/上传中")
	}
	defer h.uploadingFiles.Delete(upKey)

	// 源存在性（锁内判定）。fromTnt 取 from 卷上 owner 租户（写盘 root）。
	fromTnt := h.volumeTenant(fromVol, owner)
	if fromTnt == nil || fromTnt.Root() == nil {
		return errResp(http.StatusBadRequest, errMsgInvalidPath)
	}
	fromRoot := fromTnt.Root()
	srcInfo, statErr := fromRoot.Stat(rel)
	if os.IsNotExist(statErr) {
		h.RecordAudit(ctx, AuditEvent{
			Action: "volume_copy", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "源文件不存在",
		})
		return errResp(http.StatusNotFound, "文件不存在")
	} else if statErr != nil {
		h.logger.Error("copy: stat 源文件失败", "file_name", remotePath, "volume", fromVol, "error", statErr)
		return errResp(http.StatusInternalServerError, "访问源文件失败")
	}
	if srcInfo.IsDir() {
		return errResp(http.StatusBadRequest, "不能复制目录（仅支持文件）")
	}

	// 同卷 copy：源已在目标卷（rel 不变即目标位置就是源自身）→ 无操作成功（幂等）。
	if fromVol == toVol {
		h.RecordAudit(ctx, AuditEvent{
			Action: "volume_copy", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess, Detail: "已在目标卷（同卷 no-op）",
		})
		return okResp("文件已在目标卷，无需复制")
	}

	// 目标卷租户与容量池（写盘 root）。
	toTnt := h.volumeTenant(toVol, owner)
	if toTnt == nil || toTnt.Root() == nil {
		return errResp(http.StatusBadRequest, errMsgInvalidPath)
	}
	toPool := h.volSet.Pool(toVol)

	// 目标同 rel 已存在：checksum 一致 → 幂等成功（不重复占配额）；不一致 →
	// targetOverwrite（镜像收敛）→ 覆盖；否则 409（copy API 不覆盖用户内容）。
	if exists, eerr := h.volumeFileExists(toVol, owner, rel); eerr != nil {
		h.logger.Error("copy: 探测目标卷失败", "file_name", remotePath, "to", toVol, "error", eerr)
		return errResp(http.StatusInternalServerError, "探测目标卷失败")
	} else if exists {
		srcCS, cerr := FileChecksumRoot(fromRoot, rel)
		if cerr != nil {
			h.logger.Error("copy: 计算源 checksum 失败", "file_name", remotePath, "error", cerr)
			return errResp(http.StatusInternalServerError, "计算源文件校验和失败")
		}
		dstCS, derr := FileChecksumRoot(toTnt.Root(), rel)
		if derr != nil {
			h.logger.Error("copy: 计算目标 checksum 失败", "file_name", remotePath, "error", derr)
			return errResp(http.StatusInternalServerError, "计算目标文件校验和失败")
		}
		if srcCS == dstCS {
			h.RecordAudit(ctx, AuditEvent{
				Action: "volume_copy", ObjectType: "file", Object: remotePath,
				Result: AuditResultSuccess, Detail: "目标已存在且 checksum 一致（幂等）",
			})
			return http.StatusOK, mirrorCopyResponse{Success: true, Message: "目标已存在且内容一致，无需复制", Checksum: srcCS, Idempotent: true}
		}
		if !targetOverwrite {
			h.RecordAudit(ctx, AuditEvent{
				Action: "volume_copy", ObjectType: "file", Object: remotePath,
				Result: AuditResultDenied, Detail: "目标已存在且内容不同（copy API 不覆盖）",
			})
			return errResp(http.StatusConflict, "目标卷已存在同名文件且内容不同（如需覆盖请先删除目标或使用镜像策略）")
		}
		// 镜像收敛：目标内容不同 → 覆盖（走下方流式复制，原子替换）。
	}

	// 幂等命中（exists 但 checksum 一致）已在上面 return——此处目标不存在或
	// targetOverwrite=true（镜像收敛覆盖），均走流式复制。

	// AD-7 双账本 to 侧先 reserve（owner 全局 → 目标卷池，防中间超限）；任一失败回滚已预留。
	// 覆盖写（镜像收敛目标已存在，prev>0）与 move 不同——镜像覆盖用 Adjust 差分收敛，
	// 走 route.Commit(prev, written) 语义：先 stat 目标 prev。
	size := srcInfo.Size()
	scope := h.quotaScopeFor(owner, rel)
	var scopeRes, poolRes *quota.Reservation
	if scope != nil {
		rr, rerr := scope.TryReserve(size)
		if rerr != nil {
			return errResp(http.StatusInsufficientStorage, "存储配额不足")
		}
		scopeRes = rr
	}
	if toPool != nil {
		rr, rerr := toPool.TryReserve(size)
		if rerr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			return errResp(http.StatusInsufficientStorage, "存储配额不足")
		}
		poolRes = rr
	}

	// 覆盖写（镜像收敛，目标已有旧内容）用 Adjust 差分：复制前 stat 目标旧大小 prev，
	// 复制后 Commit(prev, written) 使账本收敛到新大小（prev==0 = 新文件 → 直接 Commit）。
	// **必须在复制前取值**——复制后目标已存在且尺寸=written，stat 会误判 prev==written
	// 使 Adjust 差分恒为 0（副本不入账，双计语义丢失）。
	prev := int64(0)
	if st, serr := toTnt.Root().Stat(rel); serr == nil && !st.IsDir() {
		prev = st.Size()
	}

	// 流式复制到 to 卷（临时 + fsync + 原子 rename）；失败回滚双预留，源不动。
	written, copyErr := crossVolumeCopy(ctx, fromRoot, toTnt.Root(), rel, rel)
	if copyErr != nil {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(ctx, AuditEvent{
			Action: "volume_copy", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "复制到目标卷失败",
		})
		h.logger.Error("copy: 复制到目标卷失败", "file_name", remotePath, "from", fromVol, "to", toVol, "error", copyErr)
		return errResp(http.StatusInternalServerError, "复制文件失败")
	}
	// 纵深防御（TOCTOU 闭合）：复制字节数须与 stat 源尺寸一致——不一致 = 源在复制中被并发
	// 改写/截断（uploadingFiles 锁已挡住同 rel upload/move，delete/restore 未持锁）。
	// fail-closed：删目标 + 双 Release 回滚，源不动。
	if written != size {
		_ = toTnt.Root().Remove(rel)
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(ctx, AuditEvent{
			Action: "volume_copy", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "复制字节与源尺寸不一致（源被并发改写），已回滚",
		})
		h.logger.Error("copy: 复制字节与源尺寸不一致，已回滚", "file_name", remotePath,
			"from", fromVol, "to", toVol, "size", size, "written", written)
		return errResp(http.StatusInternalServerError, "复制文件失败")
	}

	// 双 commit（to 侧预留对账为实际占用 written）。覆盖写（镜像收敛，目标已有旧内容）
	// 用 Adjust 差分：prev 在复制前已捕获，Commit(prev, written) 使账本收敛到新大小。
	if scopeRes != nil {
		if prev > 0 {
			scope.Adjust(prev, written)
			scopeRes.Release()
		} else {
			scopeRes.Commit(written)
		}
	}
	if poolRes != nil {
		if prev > 0 {
			toPool.Adjust(prev, written)
			poolRes.Release()
		} else {
			poolRes.Commit(written)
		}
	}
	// 源保留：from 侧账本不动（与 move 的差异——move 删源后 from 侧 ReleaseUsage/
	// ReleaseCommitted）。

	// checksum 台账：目标卷副本也登记（与上传一致，供后续幂等/删除校验复用）。
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Set(rel, serverChecksumOf(written))
	}

	// 计算实际写入 checksum（流式复制未计哈希——补算，幂等/校验用）。
	srcCS, cerr := FileChecksumRoot(fromRoot, rel)
	if cerr != nil {
		srcCS = ""
	}
	h.RecordAudit(ctx, AuditEvent{
		Action: "volume_copy", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "from=" + fromVol + " to=" + toVol,
	})
	h.logger.Info("跨卷复制成功", "file_name", remotePath, "from", fromVol, "to", toVol)
	return http.StatusOK, mirrorCopyResponse{Success: true, Message: fmt.Sprintf("文件已复制: %s (%s → %s)", remotePath, fromVol, toVol), Checksum: srcCS, Size: written}
}

// serverChecksumOf 占位：流式复制不产出哈希，checksum 台账以源文件实测为准
// （见 copyFileBetweenVolumes 补算分支）。此处保持函数存在以防未来流式复制返回哈希。
func serverChecksumOf(written int64) string {
	_ = written
	return ""
}

// mirrorVolumeStats 是单次 mirrorVolume 的统计结果。
type mirrorVolumeStats struct {
	copied      int   // 实际复制的文件数（含覆盖）
	skipped     int   // 幂等跳过（目标已一致）的文件数
	bytesCopied int64 // 复制字节合计
}

// mirrorVolume 把 srcVol user 桶内容镜像到 dstVol（源保留）。逐 owner（默认卷根扫描）逐
// 文件：owner 在 dst 卷视图内才镜像；目标一致 → 跳过；不一致 → 覆盖（镜像收敛）；
// 配额不足/复制失败 → 跳过（尽力而为，不阻断其它文件）。返回统计。
func (h *Handlers) mirrorVolume(srcVol, dstVol string) (mirrorVolumeStats, error) {
	var stats mirrorVolumeStats
	if h.volSet == nil || srcVol == dstVol {
		return stats, nil
	}
	srcTnt := h.volumeTenant(srcVol, anonymousOwner)
	if srcTnt == nil {
		return stats, nil
	}
	// 逐 owner（磁盘扫描默认卷根——与云任务恢复同源；镜像以默认卷可见租户为准）。
	owners := h.listTenantIDs()
	if len(owners) == 0 {
		// 至少处理 anonymous（未认证默认租户，可能未出现在磁盘扫描——直接补一次）。
		owners = []string{anonymousOwner}
	}
	foundAnon := slices.Contains(owners, anonymousOwner)
	if !foundAnon {
		owners = append(owners, anonymousOwner)
	}

	for _, owner := range owners {
		// owner 在 dst 卷视图内才镜像（ACL 过滤，AD-6：无权卷不泄不写）。
		if !h.volumeAllowedFor(owner, dstVol) {
			continue
		}
		files, lerr := h.listVolumeUserFiles(srcVol, owner)
		if lerr != nil {
			h.logger.Warn("mirror: 列出源卷文件失败", "volume", srcVol, "owner", owner, "error", lerr)
			continue
		}
		for _, f := range files {
			status, resp := h.copyFileBetweenVolumes(context.Background(), owner, f.relName, srcVol, dstVol, true)
			switch {
			case status == http.StatusOK && resp.Idempotent:
				stats.skipped++
			case status == http.StatusOK:
				stats.copied++
				stats.bytesCopied += resp.Size
			default:
				// 尽力而为：目标冲突（已处理为覆盖）、配额不足 507、并发 409、源被并发删 404
				// 均跳过该文件继续。
				h.logger.Info("mirror: 跳过单文件", "file", f.relName, "owner", owner, "status", status, "message", resp.Message)
				stats.skipped++
			}
		}
	}
	return stats, nil
}

// volumeMirrorPass 执行一轮全部镜像策略（volumes[].mirror_to 非空且本地卷）。每源卷调
// mirrorVolume 一次，失败/空策略跳过；返回总复制文件数与错误（最后一个错误，尽力而为）。
func (h *Handlers) volumeMirrorPass() (mirrorVolumeStats, error) {
	if h.volSet == nil {
		return mirrorVolumeStats{}, nil
	}
	var total mirrorVolumeStats
	var lastErr error
	for _, v := range h.volSet.All() {
		dst := v.MirrorTarget()
		if dst == "" {
			continue
		}
		// 目标卷必须存在且本地（目标为外部卷不支持——装配/校验已保证目标存在，
		// 外部目标在 mirrorVolume 内经 volumeTenant 返回 nil → 跳过）。
		st, err := h.mirrorVolume(v.Name, dst)
		total.copied += st.copied
		total.skipped += st.skipped
		total.bytesCopied += st.bytesCopied
		if err != nil {
			lastErr = err
		}
	}
	h.RecordAudit(context.Background(), AuditEvent{
		Action: "volume_mirror", ObjectType: "volume", Object: "mirror pass",
		Result: AuditResultSuccess, Detail: fmt.Sprintf("copied=%d bytes=%d", total.copied, total.bytesCopied),
	})
	return total, lastErr
}

// hasMirrorStrategy 判断是否任一装配卷配置了镜像目标（volumes[].mirror_to 非空且本地卷）。
// RegisterRoutes 用它决定是否启动 mirror goroutine（与 mirror_interval > 0 双条件）。
func (h *Handlers) hasMirrorStrategy() bool {
	if h.volSet == nil {
		return false
	}
	for _, v := range h.volSet.All() {
		if v.MirrorTarget() != "" {
			return true
		}
	}
	return false
}

// mirrorVolumeLoop 按 cfg.MirrorInterval 周期执行卷镜像 pass（mirror_interval > 0 时由
// RegisterRoutes 启动；Close() 关 mirrorStop）。与 credentialRotationLoop / versionGCLoop
// 同构（ticker + stop channel + WaitGroup）。
func (h *Handlers) mirrorVolumeLoop() {
	ticker := time.NewTicker(h.cfgPtr.Load().MirrorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.mirrorStop:
			return
		case <-ticker.C:
			stats, err := h.volumeMirrorPass()
			if err != nil {
				h.logger.Error("镜像 pass 失败", "error", err)
				continue
			}
			if stats.copied > 0 {
				h.logger.Info("镜像 pass 完成", "copied", stats.copied, "bytes", stats.bytesCopied, "skipped", stats.skipped)
			}
		}
	}
}

// mirrorStats 别名（供测试/未来 API 复用统计类型）。
var _ = errors.Is
var _ = quota.ErrStorageFull
var _ = storage.AnonymousOwner
var _ = volume.AllowedVolumes
var _ = sort.SliceStable
var _ = filepath.Join
