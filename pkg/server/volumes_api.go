// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_api.go 实现多卷深度 API（任务 6c / 规格 §10、§7 move）：
//   - GET /api/volumes：owner 可见卷列表（ACL per-owner，绝不可全局列卷），usage = 卷容量池 Usage。
//   - POST /api/volumes/move?from_volume&to_volume&filename：跨卷移动同 owner 同 rel。
//   - POST /api/volumes/rebalance?from_volume&to_volume&max_bytes：卷再平衡——把 from 卷
//     user 桶文件（按大小降序）逐文件迁到 to 卷，直到 max_bytes 用尽或无可迁文件。
//
// move 语义（AD-4 唯一 / AD-6 ACL / AD-7 双账本）：
//   - from/to 都须在 owner 卷视图内（否则 403）；源在 from 卷（否则 404）；目标唯一性查重
//     （视图内其它卷（含 to）已有同 rel → 409，防跨卷双份）；from==to 同卷直接无操作成功；
//   - 跨卷 = to 侧双 reserve（owner 全局 + 目标卷池，先 reserve 防中间超限）→ 流式复制到
//     to 卷临时文件 → fsync → 原子 rename → 删源 → 双 commit（to）→ 双 release（from）；
//   - 并发/幂等：同 rel 移动与上传共用 uploadingFiles 锁（同文件并发 move 恰一成功，其余 409）。
//
// rebalance 语义：逐文件复用 move 的原子单文件移动核心（moveFileBetweenVolumes），
//   单文件失败（目标已存在/配额不足/被并发迁走）跳过继续（尽力而为）；同 rel 并发
//   rebalance×move×upload 由 uploadingFiles 锁串行化（恰一成功）。remaining 为迁移后
//   from 卷池 Usage。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// VolumeStatus 是 GET /api/volumes 中单个卷的描述（仅 owner 允许卷；allowed 恒 true——
// 列表本身只含允许卷，字段保留给客户端/未来扩展）。
type VolumeStatus struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`     // 卷 ACL 模式（allow|deny）
	Capacity int64  `json:"capacity"` // 卷容量上限（0 = 不限）
	Usage    int64  `json:"usage"`    // 卷容量池当前已用字节
	Allowed  bool   `json:"allowed"`  // owner 是否允许使用（列表内恒 true）
	State    string `json:"state"`    // 健康状态：healthy|degraded|unknown（外部卷探针）
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
	view := egressVisibleView(volume.AllowedVolumes(h.volSet.All(), owner), owner)
	out := make([]VolumeStatus, 0, len(view))
	for _, v := range view {
		var usage int64
		if p := h.volSet.Pool(v.Name); p != nil {
			usage = p.Usage()
		}
		// C3：外部卷（type != local）优先用卷级计数（UsageProvider）；Pool 是本地卷容量池。
		state := volumeStateHealthy // 本地卷恒 healthy
		if v.Type != "" && v.Type != volume.TypeLocal {
			if be := h.volSet.External(v.Name); be != nil {
				if up, ok := be.(registry.UsageProvider); ok {
					usage = up.Usage()
				}
				state = h.externalVolumeState(v.Name, be)
			}
		}
		out = append(out, VolumeStatus{
			Name:     v.Name,
			Mode:     string(v.ACL.Mode),
			Capacity: v.Capacity,
			Usage:    usage,
			Allowed:  true,
			State:    state,
		})
	}
	sendJSONResponse(w, volumesListResponse{Volumes: out}, http.StatusOK)
}

// 外部卷健康状态字面量。
const (
	volumeStateHealthy  = "healthy"
	volumeStateDegraded = "degraded"
	volumeStateUnknown  = "unknown"
)

// externalVolumeState 返回外部卷健康状态（探针缓存：30s TTL 内复用上次结果，避免
// 每次列表都拨号探测）。后端未实现 HealthProbe → unknown（不误报 degraded）。
func (h *Handlers) externalVolumeState(name string, be registry.ExternalBackend) string {
	probe, ok := be.(registry.HealthProbe)
	if !ok {
		return volumeStateUnknown
	}
	h.healthMu.Lock()
	defer h.healthMu.Unlock()
	if h.externalHealth == nil {
		h.externalHealth = make(map[string]externalHealthEntry)
	}
	now := time.Now()
	if e, ok := h.externalHealth[name]; ok && now.Sub(e.at) < externalHealthTTL {
		return e.state
	}
	ctx, cancel := context.WithTimeout(context.Background(), externalProbeTimeout)
	defer cancel()
	state := volumeStateHealthy
	if err := probe.Ping(ctx); err != nil {
		state = volumeStateDegraded
		// 告警引擎挂点（roadmap P1 阈值告警）：卷 degraded 即时告警（nil = 未启用）。
		if h.alertEngine != nil {
			h.alertEngine.OnVolumeDegraded(ctx, name, err.Error())
		}
	} else if h.alertEngine != nil {
		h.alertEngine.OnVolumeRecovered(ctx, name)
	}
	h.externalHealth[name] = externalHealthEntry{state: state, at: now}
	return state
}

// externalHealthEntry 是外部卷健康状态缓存条目。
// at 是最近探测时刻；state 是结果。
type externalHealthEntry struct {
	state string
	at    time.Time
}

// externalHealthTTL 是健康探测结果缓存有效期（30s，roadmap 3.3 P1 探针周期）。
const externalHealthTTL = 30 * time.Second

// externalProbeTimeout 是单次健康探测超时（拨号/握手有界，防列表挂起）。
const externalProbeTimeout = 5 * time.Second

// removeMovedSourceOrDefault 返回 move 删源实现：实例字段（构造时注入）优先，nil → 默认
// storage.Root.Remove。**运行期不可变**——无包级 seam，并发安全（用户裁定 2026-10-07）。
func (h *Handlers) removeMovedSourceOrDefault() func(root *storage.Root, rel string) error {
	if h.removeMovedSource != nil {
		return h.removeMovedSource
	}
	return removeMovedSource
}

// removeMovedSource 是 move 删源的默认实现（storage.Root.Remove）。**非 seam**——
// 测试经 Handlers 实例字段 removeMovedSource（构造时注入）模拟「并发 delete 在 move stat
// 与 Remove 之间已删源」的 IsNotExist 竞态（真实并发时序跨平台不可确定——Windows 上源
// 文件在 move 复制期间被打开，并发 Remove 通常共享冲突失败而非 IsNotExist）。
// 用户裁定 2026-10-07：禁止测试依赖/并发修改包级全局——实例字段运行期不可变。
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

	// ACL 视图（AD-6/§8）：from/to 都必须在 owner 视图内，否则 403（不泄卷存在性）。
	if !h.volumeAllowedFor(owner, fromVol) || !h.volumeAllowedFor(owner, toVol) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return
	}

	// 单文件移动核心：ACL 校验通过后，由 moveFileBetweenVolumes 完成同 rel 的
	// 锁 + 源校验 + 唯一性查重 + 双预留 + 复制 + 删源 + commit/release。
	// 返回 (状态码, 响应体)。move 与 rebalance 共用同一原子语义（rebalance 逐文件调用）。
	status, resp := h.moveFileBetweenVolumes(r, owner, remotePath, fromVol, toVol)
	sendJSONResponse(w, resp, status)
}

// rebalanceVolumeResponse 是 POST /api/volumes/rebalance 的响应体。
type rebalanceVolumeResponse struct {
	Success    bool   `json:"success"`
	Message    string `json:"message"`
	Moved      int    `json:"moved"`
	BytesMoved int64  `json:"bytes_moved"`
	Remaining  int64  `json:"remaining"`
}

// rebalanceVolumeHandler 处理 POST /api/volumes/rebalance?from_volume&to_volume&max_bytes。
// 从 from 卷 user 桶按大小降序列出文件，逐文件复用 move 原子语义迁到 to 卷，直到
// max_bytes 用尽或无可迁文件。单文件失败跳过继续（尽力而为）；同 rel 并发由
// uploadingFiles 锁串行化。from==to → no-op 成功。remaining = 迁移后 from 卷池 Usage。
func (h *Handlers) rebalanceVolumeHandler(w http.ResponseWriter, r *http.Request) {
	fromVol := r.URL.Query().Get("from_volume")
	toVol := r.URL.Query().Get("to_volume")
	if fromVol == "" || toVol == "" {
		sendJSONResponse(w, rebalanceVolumeResponse{Success: false, Message: "from_volume、to_volume 均不能为空"}, http.StatusBadRequest)
		return
	}
	if h.volSet == nil {
		sendJSONResponse(w, rebalanceVolumeResponse{Success: false, Message: "卷功能未装配，无法卷再平衡"}, http.StatusBadRequest)
		return
	}
	owner := normalizeOwner(ownerFromRequest(r))

	// ACL 视图（AD-6/§8）：from/to 都必须在 owner 视图内，否则 403（不泄卷存在性）。
	if !h.volumeAllowedFor(owner, fromVol) || !h.volumeAllowedFor(owner, toVol) {
		sendJSONResponse(w, rebalanceVolumeResponse{Success: false, Message: msgVolumeNotAllowed}, http.StatusForbidden)
		return
	}

	// from==to：无操作成功（幂等）。
	if fromVol == toVol {
		sendJSONResponse(w, rebalanceVolumeResponse{Success: true, Message: "源卷与目标卷相同，无需再平衡"}, http.StatusOK)
		return
	}

	// max_bytes：0/空 = 不限；非法值 → 400 fail-closed。
	maxBytes, ok := rebalanceMaxBytes(r)
	if !ok {
		sendJSONResponse(w, rebalanceVolumeResponse{Success: false, Message: "max_bytes 无效"}, http.StatusBadRequest)
		return
	}

	// 列出 from 卷 user 桶全部文件（递归，含子目录），按大小降序。
	files, lerr := h.listVolumeUserFiles(fromVol, owner)
	if lerr != nil {
		h.logger.Error("rebalance: 列出 from 卷文件失败", "from", fromVol, "error", lerr)
		sendJSONResponse(w, rebalanceVolumeResponse{Success: false, Message: "读取源卷文件失败"}, http.StatusInternalServerError)
		return
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].size > files[j].size })

	// 迁移总字节（用于进度百分比）；无可迁文件时 total=0 → 进度不可算（无任务输出）。
	var totalBytes int64
	for _, f := range files {
		totalBytes += f.size
	}
	h.rebalanceProg.begin(fromVol, toVol, totalBytes)
	// 完成/失败路径统一清除进度（/metrics 不再输出该序列）。
	defer h.rebalanceProg.end(fromVol, toVol)

	moved, bytesMoved := h.rebalanceMigrateFiles(r, owner, fromVol, toVol, files, maxBytes)

	var remaining int64
	if p := h.volSet.Pool(fromVol); p != nil {
		remaining = p.Usage()
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "volume_rebalance", ObjectType: "file", Object: fromVol + "→" + toVol,
		Result: AuditResultSuccess, Detail: fmt.Sprintf("moved=%d bytes=%d", moved, bytesMoved),
	})
	sendJSONResponse(w, rebalanceVolumeResponse{
		Success:    true,
		Message:    fmt.Sprintf("卷再平衡完成: %d 个文件、%d 字节已迁移", moved, bytesMoved),
		Moved:      moved,
		BytesMoved: bytesMoved,
		Remaining:  remaining,
	}, http.StatusOK)
}

// rebalanceMaxBytes 解析 rebalance 的 max_bytes 查询参数：0/空 = 不限；非法值/负数 → 400
// fail-closed。返回 (限值, 是否合法)。
func rebalanceMaxBytes(r *http.Request) (int64, bool) {
	maxBytes := int64(0)
	if mb := r.URL.Query().Get("max_bytes"); mb != "" {
		v, perr := parseMaxBytes(mb)
		if perr != nil || v < 0 {
			return 0, false
		}
		maxBytes = v
	}
	return maxBytes, true
}

// rebalanceMigrateFiles 逐文件迁移（尽力而为）：max_bytes 用尽即停，单文件失败跳过继续。
// 返回 (成功迁移数, 迁移字节数)。
func (h *Handlers) rebalanceMigrateFiles(r *http.Request, owner, fromVol, toVol string, files []rebalanceFileEntry, maxBytes int64) (int, int64) {
	var moved int
	var bytesMoved int64
	for _, f := range files {
		// max_bytes 用尽即停（剩余配额不足当前文件 → 跳过看更小的，配额够才迁）。
		if maxBytes > 0 && maxBytes-bytesMoved < f.size {
			continue
		}
		status, resp := h.moveFileBetweenVolumes(r, owner, f.relName, fromVol, toVol)
		if status != http.StatusOK {
			// 单文件失败（目标已存在 409 / 配额不足 507 / 被并发迁走 404/409）：尽力而为跳过。
			h.logger.Info("rebalance: 跳过单文件", "file", f.relName, "status", status, "message", resp.Message)
			continue
		}
		moved++
		bytesMoved += f.size
		// 进度：已迁移字节累加（百分比由 /metrics 拉取时派生）。
		h.rebalanceProg.add(fromVol, toVol, f.size)
		if maxBytes > 0 && bytesMoved >= maxBytes {
			break
		}
	}
	return moved, bytesMoved
}

// volumeAllowedFor 判定 owner 是否被指定卷 ACL 放行（AD-6）。未知卷名 → false（fail-closed）。
func (h *Handlers) volumeAllowedFor(owner, volName string) bool {
	if h.volSet == nil {
		return false // 未装配卷集 → fail-closed（转存 ACL 拒绝）
	}
	v, ok := h.volSet.ByName(volName)
	return ok && v.Authorize(owner)
}

// moveFileBetweenVolumes 是跨卷单文件移动的原子核心（move 与 rebalance 共用）。
// 语义与 moveVolumeHandler 一致（见文件头注释）：锁 + 源校验 + 唯一性查重 + 双预留 +
// 流式复制 + fsync + 原子 rename + 删源三分支 + 双 commit/release。
// remotePath 是用户输入路径（已 ValidateFilePath）；rel 由 tnt.UserRel 派生（user 桶内）。
// 返回 (HTTP 状态码, 响应体)。
func (h *Handlers) moveFileBetweenVolumes(r *http.Request, owner, remotePath, fromVol, toVol string) (int, UploadResponse) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return moveErrResp(http.StatusBadRequest, errMsgInvalidPath)
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return moveErrResp(http.StatusBadRequest, errMsgInvalidPath)
	}

	// 并发同文件防护：同 owner+rel 移动/上传共用 uploadingFiles 锁池（与 upload handler 同键），
	// 串行化并发 move / move×upload / rebalance×move；后到者 409。锁在参数/ACL 校验之后、
	// 源校验之前获取，使「源仍存在」「目标唯一」判定在锁内完成。
	upKey := owner + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, uploadingLockMove); loaded {
		return moveErrResp(http.StatusConflict, "文件正在移动/上传中")
	}
	defer h.uploadingFiles.Delete(upKey)

	// 源存在性（锁内判定）。fromTnt 取 from 卷上 owner 租户（写盘 root）。
	srcInfo, fromRoot, status, resp, sOk := h.moveSourceCheck(r, owner, remotePath, fromVol, rel)
	if !sOk {
		return status, resp
	}

	// 同卷 move：源已在目标卷（rel 不变即目标位置就是源自身）→ 无操作成功（幂等）。
	if fromVol == toVol {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess, Detail: "已在目标卷（同卷 no-op）",
		})
		return moveOKResp("文件已在目标卷，无需移动")
	}

	// 目标唯一性（AD-4）：视图内其它卷（含 to）已存在同 rel → 409（防跨卷双份 / 静默覆盖）。
	status, resp, uOk := h.moveCheckUnique(r, owner, remotePath, fromVol, toVol, rel)
	if !uOk {
		return status, resp
	}

	// 目标卷租户与容量池（写盘 root）。
	toTnt := h.volumeTenant(toVol, owner)
	if toTnt == nil || toTnt.Root() == nil {
		return moveErrResp(http.StatusBadRequest, errMsgInvalidPath)
	}
	toRoot := toTnt.Root()
	mc := &moveFileCtx{
		r: r, owner: owner, remotePath: remotePath,
		fromVol: fromVol, toVol: toVol, rel: rel,
		fromRoot: fromRoot, toRoot: toRoot,
	}

	// AD-7 双账本 to 侧先 reserve（owner 全局 → 目标卷池，防中间超限）；任一失败回滚已预留。
	size := srcInfo.Size()
	scopeRes, poolRes, scope, status, resp, rOk := h.moveReserveTarget(owner, rel, toVol, size)
	if !rOk {
		return status, resp
	}

	// 流式复制到 to 卷（临时 + fsync + 原子 rename）；失败回滚双预留，源不动。
	written, status, resp, cOk := h.moveCopyToTarget(mc, size, scopeRes, poolRes)
	if !cOk {
		return status, resp
	}

	// 删源三分支 + 双 commit/release 结算（语义见 moveDeleteSource）。
	return h.moveDeleteSource(mc, written, scope, scopeRes, poolRes)
}

// moveOKResp 构造跨卷移动成功响应体。
func moveOKResp(msg string) (int, UploadResponse) {
	return http.StatusOK, UploadResponse{Success: true, Message: msg}
}

// moveErrResp 构造跨卷移动失败响应体。
func moveErrResp(status int, msg string) (int, UploadResponse) {
	return status, UploadResponse{Success: false, Message: msg}
}

// moveSourceCheck 在锁内探测源文件（存在性/目录判定）。不存在 → 404 + 审计；其它 stat 错误
// → 500 + 日志；目录 → 400。返回 (源信息, from 卷根, 状态码, 响应体, 是否可继续)。
func (h *Handlers) moveSourceCheck(r *http.Request, owner, remotePath, fromVol, rel string) (os.FileInfo, *storage.Root, int, UploadResponse, bool) {
	fromTnt := h.volumeTenant(fromVol, owner)
	if fromTnt == nil || fromTnt.Root() == nil {
		return nil, nil, http.StatusBadRequest, UploadResponse{Success: false, Message: errMsgInvalidPath}, false
	}
	fromRoot := fromTnt.Root()
	srcInfo, statErr := fromRoot.Stat(rel)
	if os.IsNotExist(statErr) {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "源文件不存在",
		})
		return nil, nil, http.StatusNotFound, UploadResponse{Success: false, Message: "文件不存在"}, false
	} else if statErr != nil {
		h.logger.Error("move: stat 源文件失败", "file_name", remotePath, "volume", fromVol, "error", statErr)
		return nil, nil, http.StatusInternalServerError, UploadResponse{Success: false, Message: "访问源文件失败"}, false
	}
	if srcInfo.IsDir() {
		return nil, nil, http.StatusBadRequest, UploadResponse{Success: false, Message: "不能移动目录（仅支持文件）"}, false
	}
	return srcInfo, fromRoot, 0, UploadResponse{}, true
}

// moveCheckUnique 目标唯一性查重（AD-4）：冲突 → 409 + 审计；探测失败 → 500 + 日志。
// 返回 (状态码, 响应体, 是否可继续)。
func (h *Handlers) moveCheckUnique(r *http.Request, owner, remotePath, fromVol, toVol, rel string) (int, UploadResponse, bool) {
	conflictVol, cerr := h.checkMoveTargetUnique(owner, rel, fromVol, toVol)
	if cerr != nil {
		h.logger.Error("move: 目标唯一性探测失败", "file_name", remotePath, "error", cerr)
		return http.StatusInternalServerError, UploadResponse{Success: false, Message: "探测目标卷失败"}, false
	}
	if conflictVol != "" {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "目标卷已存在同名: " + conflictVol,
		})
		return http.StatusConflict, UploadResponse{Success: false, Message: "目标卷已存在同名文件"}, false
	}
	return 0, UploadResponse{}, true
}

// moveReserveTarget 目标侧双账本预留（owner 全局 → 目标卷池，防中间超限）；任一失败回滚已预留。
// 返回 (scope 预留, 卷池预留, owner 全局 scope, 状态码, 响应体, 是否可继续)。
func (h *Handlers) moveReserveTarget(owner, rel, toVol string, size int64) (*quota.Reservation, *quota.Reservation, *quota.Scope, int, UploadResponse, bool) {
	toPool := h.volSet.Pool(toVol)
	scope := h.quotaScopeFor(owner, rel)
	var scopeRes, poolRes *quota.Reservation
	if scope != nil {
		rr, rerr := scope.TryReserve(size)
		if rerr != nil {
			return nil, nil, nil, http.StatusInsufficientStorage, UploadResponse{Success: false, Message: msgStorageQuotaExceeded}, false
		}
		scopeRes = rr
	}
	if toPool != nil {
		rr, rerr := toPool.TryReserve(size)
		if rerr != nil {
			if scopeRes != nil {
				scopeRes.Release()
			}
			return nil, nil, nil, http.StatusInsufficientStorage, UploadResponse{Success: false, Message: msgStorageQuotaExceeded}, false
		}
		poolRes = rr
	}
	return scopeRes, poolRes, scope, 0, UploadResponse{}, true
}

// moveFileCtx 是跨卷移动（move / rebalance）内部步骤共享的上下文：请求端点 + 源/目标卷名 +
// 相对路径与源/目标卷根，供 moveCopyToTarget / moveDeleteSource / moveCommitConcurrentDeleted /
// moveRollbackSourceDelete / moveCommitRelease 复用。
type moveFileCtx struct {
	r          *http.Request
	owner      string
	remotePath string
	fromVol    string
	toVol      string
	rel        string
	fromRoot   *storage.Root
	toRoot     *storage.Root
}

// moveCopyToTarget 流式复制到目标卷临时文件并校验字节数（TOCTOU 闭合）。失败/不一致 → 删目标
// + 双 Release 回滚，源不动。返回 (复制字节数, 状态码, 响应体, 是否可继续)。
func (h *Handlers) moveCopyToTarget(mc *moveFileCtx, size int64, scopeRes, poolRes *quota.Reservation) (int64, int, UploadResponse, bool) {
	written, copyErr := crossVolumeCopy(mc.r.Context(), mc.fromRoot, mc.toRoot, mc.rel, mc.rel)
	if copyErr != nil {
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(mc.r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: mc.remotePath,
			Result: AuditResultError, Detail: "复制到目标卷失败",
		})
		h.logger.Error("move: 复制到目标卷失败", "file_name", mc.remotePath, "from", mc.fromVol, "to", mc.toVol, "error", copyErr)
		return 0, http.StatusInternalServerError, UploadResponse{Success: false, Message: msgMoveFileFailed}, false
	}
	// 纵深防御（TOCTOU 闭合）：复制字节数须与 stat 源尺寸一致——不一致 = 源在复制中被并发改写/
	// 截断（uploadingFiles 锁已挡住同 rel upload/move，delete/restore 未持锁）。fail-closed：
	// 删目标 + 双 Release 回滚，源不动，不把「不确定内容」当移动成功提交。
	if written != size {
		_ = mc.toRoot.Remove(mc.rel)
		if scopeRes != nil {
			scopeRes.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
		h.RecordAudit(mc.r.Context(), AuditEvent{
			Action: "volume_move", ObjectType: "file", Object: mc.remotePath,
			Result: AuditResultError, Detail: "复制字节与源尺寸不一致（源被并发改写），已回滚",
		})
		h.logger.Error("move: 复制字节与源尺寸不一致，已回滚", "file_name", mc.remotePath,
			"from", mc.fromVol, "to", mc.toVol, "size", size, "written", written)
		return 0, http.StatusInternalServerError, UploadResponse{Success: false, Message: msgMoveFileFailed}, false
	}
	return written, 0, UploadResponse{}, true
}

// moveDeleteSource 删源三分支（详见函数内注释）与双 commit/release 结算：
//   - 成功（nil）→ 双 commit（to 侧预留对账为实际占用 written）+ from 侧释放（owner 全局
//   - from 卷池）——正常移动路径；
//   - IsNotExist = 并发 delete（不持 uploadingFiles 锁）已先删源并**已释放 from 侧账本**
//     （目标卷已持数据）→ **只 commit to 侧**、立即 return，绝不 from 侧再释放（否则 owner
//     全局 + from 卷池各欠计 S，PR-D F1）；回包按成功（源消失但数据已落目标卷，移动语义完成）；
//   - 其它错误 → 回滚 to 侧（删目标 + 释放预留），源保留。
func (h *Handlers) moveDeleteSource(mc *moveFileCtx, written int64, scope *quota.Scope, scopeRes, poolRes *quota.Reservation) (int, UploadResponse) {
	rmErr := h.removeMovedSourceOrDefault()(mc.fromRoot, mc.rel)
	if rmErr != nil {
		// IsNotExist = 并发 delete 已先删源 → 只 commit to 侧（语义见 moveCommitConcurrentDeleted）。
		if errors.Is(rmErr, os.ErrNotExist) {
			return h.moveCommitConcurrentDeleted(mc, written, scopeRes, poolRes)
		}
		// 其它错误 → 回滚 to 侧（删目标 + 释放预留），源保留。
		return h.moveRollbackSourceDelete(mc, rmErr, scopeRes, poolRes)
	}
	// 正常移动路径：双 commit + from 侧释放。
	return h.moveCommitRelease(mc, written, scope, scopeRes, poolRes)
}

// moveCommitConcurrentDeleted 删源时发现源已被并发 delete 删除（IsNotExist）：目标卷已持数据，
// **只 commit to 侧**、绝不 from 侧再释放（否则 owner 全局 + from 卷池各欠计 S，PR-D F1）；
// 回包按成功（源消失但数据已落目标卷，移动语义完成）。
func (h *Handlers) moveCommitConcurrentDeleted(mc *moveFileCtx, written int64, scopeRes, poolRes *quota.Reservation) (int, UploadResponse) {
	if scopeRes != nil {
		scopeRes.Commit(written)
	}
	if poolRes != nil {
		poolRes.Commit(written)
	}
	h.RecordAudit(mc.r.Context(), AuditEvent{
		Action: "volume_move", ObjectType: "file", Object: mc.remotePath,
		Result: AuditResultSuccess, Detail: "源已被并发删除，目标卷已持数据（from 侧账本由并发 delete 释放）",
	})
	h.logger.Info("跨卷移动完成：源已被并发删除", "file_name", mc.remotePath, "from", mc.fromVol, "to", mc.toVol)
	return moveOKResp(fmt.Sprintf("文件已移动: %s (%s → %s)", mc.remotePath, mc.fromVol, mc.toVol))
}

// moveRollbackSourceDelete 删源失败（非 IsNotExist）→ 回滚 to 侧（删目标 + 释放预留），源保留。
func (h *Handlers) moveRollbackSourceDelete(mc *moveFileCtx, rmErr error, scopeRes, poolRes *quota.Reservation) (int, UploadResponse) {
	_ = mc.toRoot.Remove(mc.rel)
	if scopeRes != nil {
		scopeRes.Release()
	}
	if poolRes != nil {
		poolRes.Release()
	}
	h.RecordAudit(mc.r.Context(), AuditEvent{
		Action: "volume_move", ObjectType: "file", Object: mc.remotePath,
		Result: AuditResultError, Detail: "删除源文件失败（已回滚目标）",
	})
	h.logger.Error("move: 删除源文件失败（已回滚）", "file_name", mc.remotePath, "error", rmErr)
	return moveErrResp(http.StatusInternalServerError, msgMoveFileFailed)
}

// moveCommitRelease 正常移动路径结算：双 commit（to 侧预留对账为实际占用 written）
// + from 侧释放（owner 全局 + from 卷池）。
func (h *Handlers) moveCommitRelease(mc *moveFileCtx, written int64, scope *quota.Scope, scopeRes, poolRes *quota.Reservation) (int, UploadResponse) {
	if scopeRes != nil {
		scopeRes.Commit(written)
	}
	if poolRes != nil {
		poolRes.Commit(written)
	}
	if scope != nil {
		scope.ReleaseUsage(written)
	}
	if fromPool := h.volSet.Pool(mc.fromVol); fromPool != nil {
		fromPool.ReleaseCommitted(written)
	}
	// M3 修复：跨卷 move 成功后在目标卷**重建 meta sidecar**、删除源卷 meta 并释放
	// 其配额——否则源卷留孤儿 .meta（磁盘+配额双泄漏）、目标卷文件无 sidecar。
	// 重建复用 filesMetaPolicy.WriteMeta（与上传到达即建同一入口，覆盖差分 + 配额记账）；
	// 源侧删除 + ReleaseUsage 与单文件删除联动同构。软删/并发删除路径不在此（moveCommitConcurrentDeleted）。
	h.moveMetaAfterVolumeMove(mc)

	h.RecordAudit(mc.r.Context(), AuditEvent{
		Action: "volume_move", ObjectType: "file", Object: mc.remotePath,
		Result: AuditResultSuccess, Detail: "from=" + mc.fromVol + " to=" + mc.toVol,
	})
	h.logger.Info("跨卷移动成功", "file_name", mc.remotePath, "from", mc.fromVol, "to", mc.toVol)
	return moveOKResp(fmt.Sprintf("文件已移动: %s (%s → %s)", mc.remotePath, mc.fromVol, mc.toVol))
}

// moveMetaAfterVolumeMove 跨卷移动后 meta sidecar 重建/清理：目标卷重建（复用
// filesMetaPolicy.WriteMeta 覆盖差分 + 配额记账）、源卷删除 meta + 释放 meta 桶配额。
func (h *Handlers) moveMetaAfterVolumeMove(mc *moveFileCtx) {
	if h.trustedDisabled() {
		return
	}
	// 目标卷重建（WriteMeta 幂等覆盖：同 rel 目标已有 meta 则 Adjust 差分；无则新 Commit）。
	if mErr := h.filesMetaWriteAfterRestore(mc.toRoot, mc.owner, mc.rel); mErr != nil {
		h.logger.Warn("跨卷移动后目标卷 meta 重建失败（读路径直算兜底）", "file_name", mc.remotePath, "error", mErr)
	}
	// 源卷删除 meta + 释放 meta 桶配额（与单文件删除联动同构）。
	srcMeta := meta.MetaPath(mc.rel)
	metaSize := int64(0)
	if e, serr := mc.fromRoot.Stat(srcMeta); serr == nil && e != nil {
		metaSize = e.Size()
	}
	_ = mc.fromRoot.Remove(srcMeta)
	if metaSize > 0 {
		if scope := h.quotaScopeFor(mc.owner, srcMeta); scope != nil {
			scope.ReleaseUsage(metaSize)
		}
	}
}

// rebalanceFileEntry 是 rebalance 候选文件（user 桶内，递归收集）。
type rebalanceFileEntry struct {
	relName string // 用户相对路径（不含 user/ 前缀，供 tnt.UserRel 反解 remotePath）
	size    int64
	mtime   time.Time // 修改时间（tier 降级按龄过滤用；rebalance 路径忽略）
}

// listVolumeUserFiles 列出指定卷 user 桶下全部文件（递归含子目录），返回用户相对路径
// （relName 不含 user/ 前缀；moveFileBetweenVolumes 用 tnt.UserRel(relName) 反解回 user 桶 rel）。
// 跳过目录与 in-flight 临时文件；统计错误 fail-closed（递归深度上界防符号链接环——os.Root
// 对中间目录强制 O_NOFOLLOW，符号链接不逃逸）。
func (h *Handlers) listVolumeUserFiles(volName, owner string) ([]rebalanceFileEntry, error) {
	tnt := h.volumeTenant(volName, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, fmt.Errorf("卷 %q 租户不可用", volName)
	}
	root := tnt.Root()
	userRoot := tnt.UserRoot()
	var out []rebalanceFileEntry
	if err := walkVolumeUserDir(root, userRoot, userRoot, 0, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkVolumeUserDir 递归遍历 user 桶目录（深度上界防符号链接环——os.Root 对中间目录强制
// O_NOFOLLOW，符号链接不逃逸），把文件条目收集到 out；rel 为 root 相对路径（初始 = userRoot）。
func walkVolumeUserDir(root *storage.Root, userRoot, rel string, depth int, out *[]rebalanceFileEntry) error {
	if depth > 100 {
		return fmt.Errorf("目录深度超过限制: %s", rel)
	}
	entries, err := root.ReadDir(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		childRel := filepath.ToSlash(filepath.Join(rel, e.Name()))
		if err := appendVolumeUserEntry(root, userRoot, childRel, e, depth, out); err != nil {
			return err
		}
	}
	return nil
}

// appendVolumeUserEntry 处理单个目录条目：目录 → 递归深入；文件 → stat 成功后收集进 out
// （用户相对路径 = rel 去掉 user/ 前缀；stat 失败/前缀不匹配跳过——尽力而为）。
func appendVolumeUserEntry(root *storage.Root, userRoot, childRel string, e os.DirEntry, depth int, out *[]rebalanceFileEntry) error {
	if e.IsDir() {
		if err := walkVolumeUserDir(root, userRoot, childRel, depth+1, out); err != nil {
			return err
		}
		return nil
	}
	info, ierr := e.Info()
	if ierr != nil {
		return nil // 单个条目 stat 失败跳过（尽力而为）
	}
	// 用户相对路径 = rel 去掉 user/ 前缀。
	relName, ok := strings.CutPrefix(childRel, userRoot+"/")
	if !ok {
		return nil
	}
	*out = append(*out, rebalanceFileEntry{relName: relName, size: info.Size(), mtime: info.ModTime()})
	return nil
}

// parseMaxBytes 解析 max_bytes 查询参数（纯数字字节；空/非法 → 错误）。
func parseMaxBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
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

// slices 导入占位（slices.SortFunc 由 ReadDir 使用——保持本文件与既有实现一致）。
