// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/transfer"
)

// cloudTaskDeps 组装 cloudtasks 的窄依赖。适配器是**无状态薄委托**（逐调用读取
// *Handlers 的当前装配态），因此逐次构造的开销可忽略，也不必在 *Handlers 上加字段
// （R7 的分域 deps 预算只有 12 个嵌入分组）。
func (h *Handlers) cloudTaskDeps() cloudtasks.Deps {
	a := &cloudTasksAdapter{h: h}
	return cloudtasks.Deps{
		Tasks:   a,
		Groups:  a,
		Tenants: a,
		Volumes: a,
		Quota:   a,
		Targets: a,
		Audit:   a,
		Logger:  h.logger,
	}
}

// cloudTaskService 构造云任务编排核心（路由注册期构造一次，之后并发复用）。
func (h *Handlers) cloudTaskService() *cloudtasks.Service {
	return cloudtasks.New(h.cloudTaskDeps())
}

// cloudTaskHandler 把编排核心的 HTTP 入口适配成 http.HandlerFunc：owner（任务归属与
// 可见性）在此从认证上下文解析——`ActorFrom` 的 ctx key 是装配层私有，核心按设计
// §4/§9.5 **显式接收 owner**（docs/designs/2026-10-31-cloudtasks-deps-mapping.md：
// 「不要把 ctx→actor 塞进领域包」）。
func (h *Handlers) cloudTaskHandler(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fn(w, r, ActorFrom(r.Context()))
	}
}

// checkTransferACL 校验转存目标卷对 owner 的 ACL（H2/R1：防跨租户覆写）。
// transfer 为 nil 或卷放行 → 返回 ""；卷未装配/ACL 拒绝 → 返回错误文案（调用方 403）。
//
// 实现随云任务 CRUD 迁入 cloudtasks（单条/批量/组三条入口共用同一判定与文案），组创建
// 仍在装配层，故保留本方法把当前装配态适配成核心的 Volumes 依赖。
func (h *Handlers) checkTransferACL(owner string, transfer *cloud.TransferSpec) string {
	return cloudtasks.TransferACLRejection(h.cloudTaskDeps().Volumes, owner, transfer)
}

// cloudCreateGroup 处理 POST /api/cloud/groups。
func (h *Handlers) cloudCreateGroup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req struct {
		Name string                `json:"name"`
		URLs []cloudfilename.Entry `json:"urls"`
		// C1：组级三参（transfer/save/download_local）——与服务端单条/批量入口对齐。
		// 组内每个子任务均按此参数创建（曾硬编码 Save:true + 无 transfer，组链三参
		// 最后一跳被丢弃：--transfer-volume 对组无效果且静默做错）。
		Transfer      *cloud.TransferSpec `json:"transfer,omitempty"`
		Save          *bool               `json:"save,omitempty"`
		DownloadLocal bool                `json:"download_local,omitempty"`
		// IntegrityMustPass 强制源文件完整性（同单条语义；组内每个子任务）。
		IntegrityMustPass bool `json:"integrity_must_pass,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := h.cloudMgr.MaxBatchURLs(); len(req.URLs) > maxBatch {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per group", maxBatch)}, http.StatusBadRequest)
		return
	}
	// 转存目标卷 ACL 校验（H2/R1：防跨租户覆写）：owner 必须被目标卷 ACL 放行。
	// 与单条/批量路径一致（组内每个子任务转存同目标卷）。
	if errMsg := h.checkTransferACL(ActorFrom(r.Context()), req.Transfer); errMsg != "" {
		sendJSONResponse(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	// 校验并规范化 URL：必须把规范化后的 URL/Filename 传给 CreateGroup，与单条/
	// 批量路径保持一致——否则同一内容的不同拼写（如 http://host/a 与 http://host/a/）
	// 在单条路径会被去重、在组路径会生成两个下载，组内文件名冲突判定也基于未
	// 规范化的值，导致与 UI/CLI 本地预检偶发不一致。
	normalized, nerr := normalizeGroupURLs(req.URLs, h.cloudMgr.AllowPrivate())
	if nerr != nil {
		sendJSONResponse(w, map[string]string{"error": nerr.Error()}, http.StatusBadRequest)
		return
	}

	group, err := h.cloudMgr.SubmitAndStartGroup(req.Name, normalized, ActorFrom(r.Context()), cloud.TaskParams{Transfer: req.Transfer, DownloadLocal: req.DownloadLocal, Save: cloudtasks.SaveOrDefault(req.Save), IntegrityMustPass: req.IntegrityMustPass})
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, groupErrorStatus(err))
		return
	}
	// 返回快照副本：SubmitAndStartGroup 返回的指针与 m.groups 共享，下载 goroutine
	// 可能在 json.Marshal 期间并发写组状态字段（UpdateGroupStatus），需副本隔离防 data race。
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "cloud_group_create", ObjectType: "task", Object: group.ID, Result: AuditResultSuccess,
		Detail: "urls=" + fmt.Sprintf("%d", len(normalized)),
	})
	snapshot, ok := h.cloudMgr.GetGroup(group.ID, ActorFrom(r.Context()))
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "group created but not found"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, snapshot, http.StatusOK)
}

// normalizeGroupURLs 校验并规范化组内每个 URL（单条/批量同款规则），返回规范化条目或错误。
// 抽离 cloudCreateGroup 控制 gocognit（gocognit=15 门禁）。
func normalizeGroupURLs(urls []cloudfilename.Entry, allowPrivate bool) ([]cloudfilename.Entry, error) {
	normalized := make([]cloudfilename.Entry, len(urls))
	for i, entry := range urls {
		cleanedURL, cleanedFilename, err := cloudtasks.ValidateCloudDownloadURL(entry.URL, entry.Filename, allowPrivate)
		if err != nil {
			return nil, err
		}
		normalized[i] = cloudfilename.Entry{URL: cleanedURL, Filename: cleanedFilename}
	}
	return normalized, nil
}

// groupErrorStatus 把组创建错误映射为 HTTP 状态码（与单条/批量路径一致；未知错误 → 500）。
func groupErrorStatus(err error) int {
	// 文件名冲突与重复 URL 均属客户端输入错误，映射 409 而非 500
	if strings.Contains(err.Error(), "filename conflict") ||
		strings.Contains(err.Error(), "duplicate URL") {
		return http.StatusConflict
	}
	// 存储不足映射 507（与单条/批量路径一致）
	if cloudtasks.IsStorageFull(err) {
		return http.StatusInsufficientStorage
	}
	// 语义空洞（save=false + download_local=false + 无 transfer = 无任何产出）是客户端
	// 输入错误 → 400（与单条路径一致：单条 CreateTask 语义空洞 fail-closed 也映射 400）。
	if strings.Contains(err.Error(), "语义空洞") {
		return http.StatusBadRequest
	}
	// 转存目标卷未装配/协议未声明/secrets 卷拒绝是配置/输入错误 → 400。
	if strings.Contains(err.Error(), "未装配") ||
		strings.Contains(err.Error(), "协议未声明") ||
		strings.Contains(err.Error(), "是 secrets 密钥卷") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// cloudGetGroup 处理 GET /api/cloud/groups/{id}。
func (h *Handlers) cloudGetGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := ActorFrom(r.Context())

	// 审查 Minor 2：先 GetGroup 校验 owner 可见性（跨 owner 立即 404，不对不可见资源
	// 执行 UpdateGroupStatus 写操作，消除计时侧信道），通过后再刷新状态并二次 GetGroup
	// 取最新快照。
	if _, ok := h.cloudMgr.GetGroup(id, owner); !ok {
		sendJSONResponse(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}
	h.cloudMgr.UpdateGroupStatus(id)
	group, ok := h.cloudMgr.GetGroup(id, owner)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 获取组详情时一并返回子任务（仅对请求者可见的子任务，IDOR 防护）。
	// 批量快照走领域 API（domain 侧一次持锁 + 同一 ownerVisible 规则），装配层不触碰
	// 领域内部状态。
	tasks := h.cloudMgr.SnapshotTasks(group.TaskIDs, owner)

	resp := map[string]any{
		"group": group,
		"tasks": tasks,
	}
	sendJSONResponse(w, resp, http.StatusOK)
}

// cloudListGroups 处理 GET /api/cloud/groups。
// 返回 {groups, total} 容器；total 为按 status 过滤后的组总数（不受分页影响）。
func (h *Handlers) cloudListGroups(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	offset, limit := cloudtasks.ParseOffsetLimit(r)
	owner := ActorFrom(r.Context())
	// 先刷新所有可见组的最新状态，再按 status 过滤返回。否则只刷新"当前已处于该状态"
	// 的组，刚转换到目标状态的组会被过滤查询漏掉，客户端看到的状态滞后。
	allGroups, _ := h.cloudMgr.ListGroups("", -1, 0, owner)
	for _, g := range allGroups {
		h.cloudMgr.UpdateGroupStatus(g.ID)
	}
	// total 需按同 status 过滤后的总数计算（ListGroups 内部过滤后统计）
	groups, total := h.cloudMgr.ListGroups(status, offset, limit, owner)
	sendJSONResponse(w, map[string]any{"groups": groups, "total": total}, http.StatusOK)
}

// cloudCancelGroup 处理 POST /api/cloud/groups/{id}/cancel。
func (h *Handlers) cloudCancelGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.CancelGroup(id, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// cloudDeleteGroup 处理 DELETE /api/cloud/groups/{id}。
// P5：组删除时联动清理组归档文件并释放 archive 桶 Scope（防孤儿归档虚高占用）。
func (h *Handlers) cloudDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := ActorFrom(r.Context())
	// 先取组快照（含 ArchiveFile），DeleteGroup 后组对象已从 map 移除无法再查。
	var archiveFile string
	if g, ok := h.cloudMgr.GetGroup(id, owner); ok {
		archiveFile = g.ArchiveFile
	}
	if err := h.cloudMgr.DeleteGroup(id, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "cloud_group_delete", ObjectType: "task", Object: id,
			Result: AuditResultError, Detail: err.Error(),
		})
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "cloud_group_delete", ObjectType: "task", Object: id, Result: AuditResultSuccess,
	})
	// 组归档文件联动删除（桶归档名或外部卷转存 URL）。失败仅记日志
	// （组删除已完成，归档残留由周期扫描校准兜底；不把归档清理失败当作组删除失败）。
	if archiveFile != "" {
		if aErr := h.deleteGroupArchive(owner, archiveFile); aErr != nil {
			h.logger.Warn("删除组归档失败（残留由周期扫描校准）", "group_id", id, "archive", archiveFile, "error", aErr)
		}
	}
	sendJSONResponse(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// cloudResumeGroup 处理 POST /api/cloud/groups/{id}/resume。
func (h *Handlers) cloudResumeGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	if err := h.cloudMgr.ResumeGroup(id, req.Force, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "resumed"}, http.StatusOK)
}

// deleteGroupArchive 删除组归档产物：外部卷转存 URL（含 "://"）→ 按卷可信 FS 删除
// （主文件 + sidecar）；否则按租户 archive 桶归档名删除（既有行为零回归）。
func (h *Handlers) deleteGroupArchive(owner, archiveFile string) error {
	if !strings.Contains(archiveFile, "://") {
		return h.deleteCloudArchive(owner, archiveFile)
	}
	u, err := url.Parse(archiveFile)
	if err != nil {
		return fmt.Errorf("解析归档 URL %q: %w", archiveFile, err)
	}
	volName := u.Host
	fsKey := strings.TrimPrefix(u.Path, "/")
	if volName == "" || fsKey == "" {
		return fmt.Errorf("归档 URL %q 缺少卷/路径", archiveFile)
	}
	fs, _, _, ok := h.externalVolumeTransferFS(volName)
	if !ok {
		return fmt.Errorf("归档目标卷 %q 未装配", volName)
	}
	// **配额释放（与写侧同源）**：组归档转存外部卷的产物在写入时经 `settleArchiveCharge` 计入
	// owner 账本（P2 修复，2026-10-21：此前删除路径只删对象、不释放 ⇒ 配额虚高，可能误 507；
	// 虽然磁盘对账会在 ≤30min 内冲掉，但属于可避免的窗口）。删除前**实测**记账字节（与写侧
	// `archiveTargetCharge` 同一函数 ⇒ 不会「未曾提交的释放」），删除成功后按同一 Scope 释放。
	charge := h.archiveTargetCharge(volName, owner, fsKey, userRelFromFSKey(owner, fsKey))
	if err := fs.Delete(context.Background(), fsKey); err != nil {
		return fmt.Errorf("删除外部卷归档 %q: %w", fsKey, err)
	}
	if charge > 0 {
		if scope := h.quotaScopeFor(owner, path.Join("user", userRelFromFSKey(owner, fsKey))); scope != nil {
			scope.ReleaseUsage(charge)
		}
		// **卷级容量释放**（独立复审 Important-1）：本地卷产物写侧已补卷池 Adjust ⇒
		// 删除侧同源释放（pool.ReleaseCommitted）。外部卷（CapacityFS 层已记）不双扣。
		if h.isLocalStorageVolume(volName) {
			if pool := h.volSet.Pool(volName); pool != nil {
				pool.ReleaseCommitted(charge)
			}
		}
	}
	return nil
}

// settleVolumePoolCharge 把归档转存产物计入**本地卷**的卷级容量池（差分；外部卷跳过——
// 其卷级记账在 FS 层 CapacityFS，双记会重复占用）。
func (h *Handlers) settleVolumePoolCharge(volName string, before, after int64) {
	if !h.isLocalStorageVolume(volName) || after <= before {
		return
	}
	if pool := h.volSet.Pool(volName); pool != nil {
		pool.Adjust(before, after)
	}
}

// isLocalStorageVolume 报告卷是否为**本地根形态**（卷级容量权威在路由层 Pool；
// 外部卷的卷级记账在 FS 层 CapacityFS，不在此处补）。
func (h *Handlers) isLocalStorageVolume(volName string) bool {
	return h.volSet != nil && h.volSet.HasLocalRoot(volName)
}

// userRelFromFSKey 把外部卷内部键 `<owner>/user/<rel>` 还原为用户桶相对路径 `<rel>`
// （`archiveTargetCharge` 的 PhysFootprint 需要该口径；与写侧 `archiveTargetRel` 同键空间）。
// 形态不符（非 `<owner>/user/` 前缀）时原样返回，交由调用方的足迹/Stat 兜底。
func userRelFromFSKey(owner, fsKey string) string {
	prefix := owner + "/user/"
	if after, ok := strings.CutPrefix(fsKey, prefix); ok {
		return after
	}
	if rest, ok := strings.CutPrefix(fsKey, "user/"); ok {
		return rest
	}
	return fsKey
}

// cloudArchiveGroup 处理 POST /api/cloud/groups/{id}/archive。// 收集组内所有已完成子任务的文件打包为单个 tar.gz（未完成任务跳过并记录）。
func (h *Handlers) cloudArchiveGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	owner := ActorFrom(r.Context())
	if _, ok := h.cloudMgr.GetGroup(groupID, owner); !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 解析请求体
	req, ok := h.cloudArchiveParseRequest(w, r)
	if !ok {
		return
	}

	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName, ok := h.cloudArchiveResolveName(w, req, groupID)
	if !ok {
		return
	}

	// 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户 cloud/ 桶）
	groupFiles, skippedTasks, totalSourceSize, ok := h.cloudArchiveCollectFiles(w, groupID, owner)
	if !ok {
		return
	}

	if len(groupFiles) == 0 {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "no completed files to archive in group",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return
	}

	// 统一转存：target_volume 非空 → 打包后转存到外部卷（复用 pkg/transfer 抽象），
	// 不落租户 archive 桶。否则落桶（既有行为零回归）。
	// V4：转存目标卷须经 ACL 校验（volumeAllowedFor 防跨租户写私有卷）——与 cloud
	// download transfer 的 checkTransferACL 同语义，此处直接复用。
	// target_path/encrypt/cipher/compression 仅与 target_volume 同用（防静默忽略）。
	if !h.validateArchiveExtras(w, archiveExtras{
		TargetVolume: req.TargetVolume, TargetPath: req.TargetPath,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
	}) {
		return
	}
	if req.TargetVolume != "" {
		if !h.volumeAllowedFor(owner, req.TargetVolume) {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
			return
		}
		h.cloudArchiveGroupToVolume(w, r, groupID, owner, groupFiles, req, archiveName)
		return
	}
	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	root, rel, ok := h.cloudArchivePrepTarget(w, owner, req, archiveName)
	if !ok {
		return
	}

	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	res, pre, ok := h.cloudArchiveReserveQuota(r.Context(), w, owner, totalSourceSize)
	if !ok {
		return
	}

	// 多文件打包
	checksum, ok := h.cloudArchiveCreateTar(w, groupFiles, root, rel, res, pre, groupID)
	if !ok {
		return
	}

	// 按磁盘实际大小对账预留配额（与单任务/批量归档同一实现：Reconcile(actual) + Commit()，
	// 多预留部分自动归还；storageMgr 回退保留 3 步 Release(pre)+TryReserve(actual)）。
	actual, ok := h.cloudArchiveSettleQuota(w, root, rel, res, pre, h.logger.With("group_id", groupID))
	if !ok {
		return
	}

	// P5 归档占用登记（删除时按登记释放 Scope，不再依赖周期扫描自愈）。
	h.recordArchiveUsage(owner, archiveName, actual)

	// 更新组归档路径（落库到真实组对象；仅存归档名，客户端不接触 .__ 内部路径）
	archiveFile := archiveName
	h.cloudMgr.SetGroupArchiveFile(groupID, archiveFile)

	sendJSONResponse(w, CloudArchiveResult{
		Success:      true,
		File:         archiveFile,
		Size:         actual,
		Checksum:     checksum,
		TaskCount:    len(groupFiles),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// cloudArchiveParseRequest 解析归档请求体（限 1 MiB + bodyValidator EOF 哈希校验）。
// 返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveParseRequest(w http.ResponseWriter, r *http.Request) (*CloudArchiveRequest, bool) {
	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req CloudArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return nil, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return nil, false
	}
	return &req, true
}

// cloudArchiveResolveName 确定归档文件名（默认使用 groupID 保证唯一性；路径穿越防护 +
// 长度校验 + 补 .tar.gz 后缀）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveResolveName(w http.ResponseWriter, req *CloudArchiveRequest, groupID string) (string, bool) {
	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("%s-%d.tar.gz", groupID, time.Now().Unix())
	}
	return h.normalizeArchiveName(w, archiveName)
}

// cloudArchiveCollectFiles 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户
// cloud/ 桶；未完成任务跳过并记录；返回 false 表示已回错误响应包）。
func (h *Handlers) cloudArchiveCollectFiles(w http.ResponseWriter, groupID, owner string) ([]fileWithRelPath, []string, int64, bool) {
	// 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户 cloud/ 桶）
	var groupFiles []fileWithRelPath
	var skippedTasks []string
	var totalSourceSize int64

	group, ok := h.cloudMgr.GetGroup(groupID, owner)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return nil, nil, 0, false
	}
	for _, taskID := range group.TaskIDs {
		task, found := h.cloudMgr.SnapshotTask(taskID, owner)
		if !found {
			h.logger.Warn("cloud group archive: skipping task not found", "group_id", groupID, "task_id", taskID)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		if task.Status != "completed" {
			h.logger.Warn("cloud group archive: skipping task with non-completed status",
				"group_id", groupID, "task_id", taskID, "status", task.Status)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）。
		// FeatureRel 逐段校验（fail-closed，防 task.Filename 穿越任务目录）。
		srcTnt := h.tenantFor(task.Owner)
		if srcTnt == nil {
			h.logger.Warn("cloud group archive: skipping task with unavailable tenant",
				"group_id", groupID, "task_id", taskID, "owner", task.Owner)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		srcRel, relOK := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
		if !relOK {
			h.logger.Warn("cloud group archive: skipping task with invalid source path",
				"group_id", groupID, "task_id", taskID, "file", task.Filename)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		// 收集后、打包前文件可能被删除/替换，先确认存在并统计大小（用于总量限制与配额预估）
		if info, statErr := srcTnt.Root().Stat(srcRel); statErr != nil {
			h.logger.Warn("cloud group archive: skipping missing file",
				"group_id", groupID, "task_id", taskID, "rel", srcRel, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
		} else {
			totalSourceSize += info.Size()
		}
		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		groupFiles = append(groupFiles, fileWithRelPath{root: srcTnt.Root(), rel: srcRel, tarRel: relPath})
	}
	return groupFiles, skippedTasks, totalSourceSize, true
}

// cloudArchivePrepTarget 确保输出目录存在并解析归档目标相对路径（租户 archive 桶），
// 含用户指定名时的同名冲突检查。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchivePrepTarget(w http.ResponseWriter, owner string, req *CloudArchiveRequest, archiveName string) (*storage.Root, string, bool) {
	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	tnt := h.tenantFor(owner)
	if tnt == nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	root := tnt.Root()
	if mkErr := root.MkdirAll("archive", 0755); mkErr != nil {
		h.logger.Error(msgArchiveDirFail, "error", mkErr)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return nil, "", false
	}
	// 用户指定归档名时，校验同名文件是否已存在，存在则拒绝（落在租户 archive 桶下，
	// 审查 F3 语义保留：不查全局根，避免误 409）
	if req.ArchiveName != "" {
		if _, err := root.Stat(rel); err == nil {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists: " + archiveName}, http.StatusConflict)
			return nil, "", false
		}
	}
	return root, rel, true
}

// cloudArchiveMaxBytesOK 校验云归档总量上限（cloud_archive_max_bytes；0=不限）。失败已回响应。
// 单任务/批量/组归档与统一转存（不占 archive 桶）共用同一上限校验。
func (h *Handlers) cloudArchiveMaxBytesOK(w http.ResponseWriter, totalSize int64) bool {
	maxBytes := h.cloudArchiveMaxBytes()
	if maxBytes > 0 && totalSize > maxBytes {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSize, maxBytes),
		}, http.StatusBadRequest)
		return false
	}
	return true
}

// cloudArchiveReserveQuota 打包前做总量限制（cloud_archive_max_bytes）+ 配额预留
// （scope 优先；storageMgr 仅作回退）。返回的结算句柄非 nil = scope 路径已预留 pre 字节
// （nil = 回退路径或未装配账本，无预留需定案），调用方必须以 cloudArchiveSettleQuota（成功）
// 或 releaseArchiveReservation（失败）定案，否则预留泄漏。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveReserveQuota(ctx context.Context, w http.ResponseWriter, owner string, totalSourceSize int64) (*quota.Settlement, int64, bool) {
	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	if !h.cloudArchiveMaxBytesOK(w, totalSourceSize) {
		return nil, 0, false
	}
	pre := totalSourceSize + cloudArchiveReservePlaceholder
	// P5 收敛：双轨 TryReserve（storageMgr + Scope 同时预留同量字节）改为二选一——
	// 生产环境 scope 恒非 nil（全局兜底由 Scope 父链生效），storageMgr 仅作回退。
	// P1-b：scope 路径的「预留 → 对账 → 提交/回滚」改走 quota.Settlement（协议唯一实现）；
	// ReserveWait 保留「容量临时不足 ⇒ 排队等释放」的能力（#963 锁定的语义，不得退化为立即 507）。
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil {
		st := quota.NewSettlement(scope.Pool())
		// h.quotaWaitTimeout = 0（生产恒 0）⇒ ReserveWait 内部按 quota.DefaultWaitTimeout（30s），
		// 语义不变。此前此处恒传 quota.DefaultWaitTimeout，测试注入的等待上限被忽略（#963 记录）。
		if reserveErr := st.ReserveWait(ctx, pre, h.quotaWaitTimeout); reserveErr != nil {
			// 失败时账本与句柄状态均不变（ReserveWait 契约）⇒ 直接丢弃句柄，无预留泄漏。
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
			}, http.StatusInsufficientStorage)
			return nil, 0, false
		}
		return st, pre, true
	} else if h.storageMgr != nil {
		if reserveErr := h.storageMgr.WaitReserve(ctx, pre, capacity.CategoryCloud, capacity.DefaultWaitTimeout); reserveErr != nil {
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
			}, http.StatusInsufficientStorage)
			return nil, 0, false
		}
	}
	// 回退路径（storageMgr）或未装配账本：无结算句柄需定案（回退预留按 pre 释放）。
	return nil, pre, true
}

// cloudArchiveCreateTar 执行多文件打包（流式 checksum；O_EXCL 同名冲突与失败路径释放
// 预留并清理半成品）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveCreateTar(w http.ResponseWriter, groupFiles []fileWithRelPath, root *storage.Root, rel string, res *quota.Settlement, pre int64, groupID string) (string, bool) {
	// 多文件打包
	created := false
	logger := h.logger.With("archive", "group", "group_id", groupID)
	checksum, err := createMultiFileTarGz(groupFiles, root, rel, logger, &created)
	if err != nil {
		if !created {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			releaseArchiveReservation(res, h.storageMgr, pre)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return "", false
		}
		h.logger.Error("failed to create group archive", "group_id", groupID, "error", err)
		_ = root.Remove(rel)
		releaseArchiveReservation(res, h.storageMgr, pre)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return "", false
	}
	return checksum, true
}

// cloudArchiveGroupToVolume 统一转存：组归档打包后转存到外部卷（target_volume）。
// 收集组文件 Reader 后交共享核心 cloudArchiveToVolume（可信卷写视图 + owner 前缀 +
// staging/写锁）。
func (h *Handlers) cloudArchiveGroupToVolume(w http.ResponseWriter, r *http.Request, groupID, owner string, groupFiles []fileWithRelPath, req *CloudArchiveRequest, archiveName string) {
	entries, err := archiveEntries(groupFiles)
	if err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "open source failed: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	// 成功时登记组归档引用（供 cloudDeleteGroup 联动清理外部产物；转存 URL 含卷信息）。
	onSuccess := func(url string) {
		if h.cloudMgr != nil {
			h.cloudMgr.SetGroupArchiveFile(groupID, url)
		}
	}
	h.cloudArchiveToVolume(w, r, archiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: archiveName,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: archiveTotalSize(groupFiles),
	}, h.logger.With("archive", "group", "group_id", groupID), onSuccess)
}

// archiveVolumeTransfer 是统一转存的入参（S107 收敛：字段替代长参数列表）。
type archiveVolumeTransfer struct {
	Owner        string
	TargetVolume string
	TargetPath   string
	ArchiveName  string
	Encrypt      bool
	Cipher       string
	Compression  string
	Entries      []transfer.SourceEntry
	TotalSize    int64
}

// cloudArchiveToVolume 是统一转存的**共享执行核心**（单任务 / 批量 / 组归档共用）：
// 把打包流（tar[.ext][.aes]）转存到外部卷。
//
// 接线纪律（复用 master 可信卷能力，与 cloud 下载转存同款）：
//   - 目标 FS = externalVolumeTransferFS（trusted.Guard(Wrap(...))）——meta 恒生成 +
//     meta 桶隔离（裸 FS 会让转存产物不是可信卷、且可触达 meta 凭据桶）；
//   - 目标键经 volume.ResolveLocation 唯一入口计算（恒 owner 前缀 + 路径安全）——与读
//     路径 ResolveUserLocation 同键空间，产物可经 /download 命中（外部卷不进本地文件
//     索引，故不出现在 /api/files 列表）；
//   - 预处理（压缩 / 可选加密）经 transfer registry 选型；
//   - 本地 staging 配额 + per-(owner,rel) 写锁（与 HTTP 上传/DAV/S3 同键空间）。
//
// t.Entries 已由调用方打开 source Reader（本函数负责关闭）；t.TotalSize 用于
// cloud_archive_max_bytes 上限校验。外部卷不消耗租户 archive 桶（产物落目标卷，容量
// 由目标卷 ReserveSpace / staging 配额管），故不做 archive 桶 Scope 预留。
func (h *Handlers) cloudArchiveToVolume(
	w http.ResponseWriter, r *http.Request,
	t archiveVolumeTransfer,
	logger *slog.Logger,
	onSuccess func(url string),
) {
	defer closeEntries(t.Entries)
	// 预处理器选型（压缩 + 可选加密，经 transfer registry）——失败已回响应。
	pre, ok := h.archivePreprocessor(w, t.Encrypt, t.Cipher, t.Compression)
	if !ok {
		return
	}
	// 目标卷解析（可信卷写视图；未装/元数据不可解析 → fail-closed）。
	// U3：本地卷现也可作转存目标（`externalVolumeTransferFS` 已改走 `VolumeView`，
	// 本地卷返回 Guard(Wrap(NewRootFS))——trusted 对本地启用）；scheme 空按 local 归一。
	fs, scheme, vol, ok := h.externalVolumeTransferFS(t.TargetVolume)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "target volume not assembled"}, http.StatusNotFound)
		return
	}
	if scheme == "" {
		scheme = "local"
	}
	// NM6（与 cloud 下载转存 checkTransferVolumePreflight 一致）：secrets 是密钥管理卷
	// （secret 名禁路径分隔符），非通用转存目标——转存 rel 恒含 /，secrets:// 无法解析
	// 且写路径绕过 Secrets Manager 校验。
	if scheme == "secrets" {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "目标卷是 secrets 密钥卷（非转存目标，请用 secretdata/普通卷）"}, http.StatusBadRequest)
		return
	}
	// 目标键：volume 唯一入口（权限门 + 路径安全 + 恒 owner 前缀），与读路径
	// resolveExternalDownload 的 ResolveUserLocation 同键空间。targetPath 为客户端由卷 URI
	// （cd/mv 同规约）归一后的 user 桶相对路径（可含子目录）；空 → 默认 archive/<name>。
	// 加密产物按 /api/archive 同口径加 .aes 后缀（`<name>.tar.gz.aes`）——密文不该伪装成
	// 可直接 gunzip 的 .tar.gz。
	archiveName := t.ArchiveName
	if t.Encrypt && !strings.HasSuffix(archiveName, ".aes") {
		archiveName += ".aes"
	}
	loc, lerr := vol.ResolveUserLocation(t.Owner, archiveTargetRel(t.TargetPath, archiveName))
	if lerr != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid target path: " + lerr.Error()}, http.StatusBadRequest)
		return
	}
	rel := loc.FSPath()
	// 本地 staging 配额统一接线（与 cloud 下载转存一致；无独立配额 → 直通零回归）。
	// 打包流长度未知（size=0）由 gate 的 reservingReader 按实际读取量增量预留（#767 原生
	// 支持）——不得再在装配层额外预留（会双计）。
	fs = h.stagingQuotaFS(t.Owner, fs)
	// 总量上限校验（不占租户 archive 桶 → 不做桶 Scope 预留，避免桶近满时误拒）。
	if !h.cloudArchiveMaxBytesOK(w, t.TotalSize) {
		return
	}
	// 容量预检（ReserveSpace 装饰器链未实现 → ErrUnsupported 跳过）+ per-(owner,rel) 写锁（与
	// HTTP 上传/删除/改名/DAV/S3 同键空间）——转存写可信卷（主文件 + sidecar meta）与其它
	// 写面并发同 rel 会产出错位固化态。
	release, status, msg := h.archiveVolumeWriteGate(r.Context(), fs, t.Owner, rel, t.TotalSize)
	if status != 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return
	}
	defer release()
	// owner 配额结算的**前置测量**（独立审核 P1，2026-10-18）：写成功后必须按**与删除侧同一
	// Scope 解析器 + 同一物理足迹测量函数**结算——否则删除该产物时 releaseExternalDeleteQuota
	// 会做「**未曾提交的释放**」（幻影释放 ⇒ 扣同账本其它占用 ⇒ 少计 ⇒ 可超 owner 配额）。
	// 键空间与 cloud 下载转存一致：Scope 键 = `<bucket>/<path>`（即 `user/<rel>`）。
	chargeScope := h.quotaScopeFor(t.Owner, path.Join(string(loc.Bucket()), string(loc.Path())))
	// R14 C6：**写前配额探针**（探测式：预留后立即释放，不排队）。此前本路径只做写后
	// `settleArchiveCharge`→`scope.Adjust`（永不失败）⇒ 配额已满时**仍会写入并越界**。
	// 对照 cloud 下载转存（`pkg/cloud/transfer.go` 的 ProbeReserve）同口径。
	// TotalSize 是源可见总量（上界近似）；未知（0/负）时跳过探针，由写路径自身兜底。
	if perr := probeArchiveQuota(chargeScope, t.TotalSize); perr != nil {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "目标卷配额不足: " + perr.Error(),
		}, http.StatusInsufficientStorage)
		return
	}
	chargeUserRel := archiveTargetRel(t.TargetPath, archiveName)
	chargeBefore := h.archiveTargetCharge(t.TargetVolume, t.Owner, rel, chargeUserRel)
	url, terr := transfer.Execute(r.Context(), transfer.TransferRequest{
		Entries:     t.Entries,
		Preprocess:  pre,
		Destination: fs,
		Scheme:      scheme,
		Volume:      t.TargetVolume,
		Rel:         rel,
		// 写后读回校验受既有 trusted_volume.skip_verify 约束（独立复审 P1，2026-10-18）：
		// 该开关的文档（docs/config.md）与启动告警（volumes.go warnIfVerifySkipped）都宣称覆盖
		// 「下载/**转存**读侧数据校验」，但本轮新增的内核读回默认执行 ⇒ 对远端卷（OpenRead =
		// 一次全量下载）无法关闭、且文档与行为矛盾。此处接线到同一配置源（与 cloud 转存一致）。
		SkipVerify: h.verifySkipped(),
	})
	// 统一转存审计（F8）：本路径是**同步 HTTP**（不经 cloud manager ⇒ 无任务级审计作用域 /
	// download·transfer span）。按备份等旁路写面的既有口径补一条服务端审计事件，保证
	// 「谁在何时把什么转存到哪个卷」可经 /api/audit 追溯。
	h.recordArchiveTransferAudit(r.Context(), t, scheme, rel, len(t.Entries), terr)
	if terr != nil {
		// 目标已存在（拒绝静默覆盖）是客户端可解冲突 → 409（非 500）。
		if errors.Is(terr, transfer.ErrExists) {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: terr.Error()}, http.StatusConflict)
			return
		}
		logger.Error("unified transfer to volume failed", "volume", t.TargetVolume, "error", terr)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "transfer failed: " + terr.Error()}, http.StatusInternalServerError)
		return
	}
	// 写成功 ⇒ 计入 owner 配额（差分：覆盖写用 before/after）。
	chargeAfter := h.archiveTargetCharge(t.TargetVolume, t.Owner, rel, chargeUserRel)
	settleArchiveCharge(chargeScope, chargeBefore, chargeAfter)
	// **卷级容量记账**（独立复审 Important-1，2026-10-28）：本地卷 rootfs 无 ReserveSpace
	// ⇒ 转存写不经 CapacityFS ⇒ 卷池漏记 ⇒ 长期漂移。按与 owner Scope **同源**的实测差分
	// 结算卷池（同一 `archiveTargetCharge` 测量）。外部卷（CapacityFS 层已记）不双记。
	h.settleVolumePoolCharge(t.TargetVolume, chargeBefore, chargeAfter)
	// **派生状态登记**（R16 缺陷族修复 B）：归档转存产物写目标卷 user 桶不经 pkg/files 写内核
	// ⇒ 需显式登记 checksum 台账 + 搜索索引（checksum 从可信卷 meta sidecar 读；仅本地卷目标
	// ——外部卷 list 走卷视图，checksum 由卷自报 `sync.Entry.Checksum` 透传）。
	h.recordArchiveBypassState(t, fs, rel, chargeAfter)
	if onSuccess != nil {
		onSuccess(url)
	}
	sendJSONResponse(w, CloudArchiveResult{Success: true, File: url, TaskCount: len(t.Entries)}, http.StatusOK)
}

// recordBypassWrite 是云转存/归档转存产物派生状态的**统一入口**（装配层注入 cloud manager）。
//
// 分流依据是「该卷是否本地根」：
//   - 本地卷：结构/内容写事件已维护搜索索引 ⇒ 只补 checksum 台账（`RecordBypassChecksum`）；
//   - 外部/远端卷：无本地 Root 事件 ⇒ 走 `RecordBypassWrite`（checksum + 索引一次登记）。
//
// 这样本地卷不再重复 upsert（省一次整表 COW 克隆），也不会为「卷内并不存在的路径」造幽灵条目。
func (h *Handlers) recordBypassWrite(owner, rel string, size int64, checksum, volume string, root *storage.Root) {
	if h.isLocalStorageVolume(volume) {
		h.fileService().RecordBypassChecksum(owner, rel, checksum, volume)
		return
	}
	h.fileService().RecordBypassWrite(owner, rel, size, checksum, volume, root)
}

// recordArchiveBypassState 归档转存（本地卷目标）的派生状态登记（R16 缺陷族修复 B）。
// V4：索引改由 `storage.Root` 事件自动维护 ⇒ 这里只补 checksum 台账。
func (h *Handlers) recordArchiveBypassState(t archiveVolumeTransfer, fs syncpkg.FS, rel string, size int64) {
	if !h.isLocalStorageVolume(t.TargetVolume) {
		return
	}
	cs := ""
	if pv, ok := fs.(meta.Provider); ok {
		if fm, ferr := pv.FileMeta(context.Background(), rel); ferr == nil && fm != nil {
			cs = fm.TotalSHA256
		}
	}
	// 本地卷：索引已由 Root 事件（结构 + 内容写）维护 ⇒ 只补 checksum（避免重复 upsert）。
	h.fileService().RecordBypassChecksum(t.Owner, rel, cs, t.TargetVolume)
}

// probeArchiveQuota 是归档转存的**写前配额探针**（R14 C6）：探测式（预留后立即释放、不排队）。
// 抽成函数以避免给 cloudArchiveToVolume 增加分支（gocognit 门禁）。scope 为 nil（未装配配额）
// 或 size ≤ 0（打包流真实长度不可知）时跳过 —— 由写路径自身兜底。
func probeArchiveQuota(scope *quota.Scope, size int64) error {
	if scope == nil || size <= 0 {
		return nil
	}
	return scope.ProbeReserve(size)
}

// settleArchiveCharge 把归档转存产物计入 owner 配额。before = 写入前实测、after = 写入后实测
// （覆盖写按差分，不双计）；scope 为 nil（未装配配额）或 after == before ⇒ 无操作（零回归）。
//
// **必须与删除侧同源**（独立审核 P1，2026-10-18）：删除侧 releaseExternalDeleteQuota 会从同一
// Scope 释放 physChargeFor 实测字节；若写侧不结算，那次释放就是「**未曾提交的释放**」⇒ 扣掉同
// 账本其它对象的占用 ⇒ 少计 ⇒ 后续写入沿父链检查时放行 ⇒ **可超出 owner 配额**。
func settleArchiveCharge(scope *quota.Scope, before, after int64) {
	if scope == nil || after == before {
		return
	}
	scope.Adjust(before, after)
}

// archiveTargetCharge 返回归档转存产物的**记账字节**：① 卷自报物理足迹优先（与删除侧
// files.physChargeFor 同一测量函数 filesPhysFootprinter.PhysFootprint）——secretdata 的
// pad/parity 逻辑不可推，两处必须同源；② 无足迹能力（多数外部卷）⇒ 回落卷内实测大小
// （与删除侧 ExternalStat 回落同档）。fsRel 是卷内完整键（`<owner>/user/<rel>`），userRel 是
// 用户桶相对路径（足迹解析用）。
func (h *Handlers) archiveTargetCharge(volName, owner, fsRel, userRel string) int64 {
	if n, ok := (filesPhysFootprinter{h: h}).PhysFootprint(volName, owner, userRel); ok && n > 0 {
		return n
	}
	fs, _, _, ok := h.externalVolumeTransferFS(volName)
	if !ok || fs == nil {
		return 0
	}
	if e, err := fs.Stat(context.Background(), fsRel); err == nil && e != nil {
		return e.Size
	}
	return 0
}

// archiveVolumeWriteGate 是统一转存的写前门：目标卷容量预检（ReserveSpace 可选能力，
// 装饰器链未实现 → ErrUnsupported 跳过）+ 转存取锁。取锁经 h.transferWriteLock（唯一
// 归一入口：卷 FS 键 → 域侧锁键）——与 HTTP 上传/删除/改名/DAV/S3、cloud 下载转存**同一
// 键空间**（否则共享外部卷上互不可见，同 rel 并发写产出主文件/sidecar 错位固化）。
// 返回 (release, status, msg)：status==0 表示通过（release 必非 nil）；否则按 status/msg 回包。
// 抽离以控制 cloudArchiveToVolume 的认知复杂度（gocognit 门禁）。
//
// size 语义：打包流的**真实产物长度打包前不可知**，此处以源可见总量作上界近似（预检，
// 非预留，无需释放）；权威强制在写路径本身——capacity.CapacityFS 按实际写入记账，
// 外部卷后端按自身配额 API 拒写。
func (h *Handlers) archiveVolumeWriteGate(ctx context.Context, fs syncpkg.FS, owner, rel string, size int64) (func(), int, string) {
	if rs, ok := fs.(syncpkg.ReserveSpace); ok {
		if err := rs.ReserveSpace(ctx, rel, size); err != nil && !errors.Is(err, syncpkg.ErrUnsupported) {
			return func() {}, http.StatusInsufficientStorage, "目标卷容量不足: " + err.Error()
		}
	}
	release, locked := h.transferWriteLock(owner, rel)
	if !locked {
		return func() {}, http.StatusConflict, "目标正被其他写者占用，请稍后重试"
	}
	return release, 0, ""
}

// recordArchiveTransferAudit 记录统一转存（归档 → 外部卷）的服务端审计事件（F8）。
// 同步 HTTP 路径没有 cloud 任务级审计作用域（见 cloudArchiveToVolume），以服务端审计行补齐
// 「谁把什么转存到哪个卷」的可追溯性——与 backup 旁路写面同口径（recordBackupAudit）。
// 文案只含卷名/键/scheme 等非敏感结构信息，不含 URL/凭据。
func (h *Handlers) recordArchiveTransferAudit(ctx context.Context, t archiveVolumeTransfer, scheme, rel string, count int, terr error) {
	result, detail := AuditResultSuccess, fmt.Sprintf("scheme=%s key=%s files=%d encrypt=%t", scheme, rel, count, t.Encrypt)
	if terr != nil {
		result = AuditResultError
		detail += " error=" + terr.Error()
	}
	h.RecordAudit(ctx, AuditEvent{
		Action: "cloud_archive", ObjectType: "volume", Object: t.TargetVolume,
		Result: result, Detail: detail,
	})
}

// archiveEntries 把 groupFiles 转为 SourceEntry（打开源文件作 Reader；size/mtime 填真值）。
// 打开中途失败时关闭已打开的 Reader（防句柄泄漏）。
func archiveEntries(files []fileWithRelPath) ([]transfer.SourceEntry, error) {
	out := make([]transfer.SourceEntry, 0, len(files))
	for _, f := range files {
		// R25：用户可见读入口（加密卷透明解密）——不得直调 root.Open。
		rc, err := rootfs.OpenUserVisibleRead(f.root, f.rel)
		if err != nil {
			closeEntries(out)
			return nil, err
		}
		st, serr := f.root.Stat(f.rel)
		var size, mtime int64
		// 用户可见（逻辑）长度：加密卷取明文长度，否则等同 Stat。
		if sv, vErr := rootfs.UserVisibleSize(f.root, f.rel); vErr == nil {
			size = sv
		} else if serr == nil && st != nil {
			size = st.Size()
		}
		if serr == nil && st != nil {
			mtime = st.ModTime().Unix()
		}
		out = append(out, transfer.SourceEntry{Rel: f.tarRel, Size: size, ModTime: mtime, Reader: rc})
	}
	return out, nil
}

// closeEntries 关闭所有 SourceEntry 的 Reader（io.Closer）。
func closeEntries(entries []transfer.SourceEntry) {
	for _, e := range entries {
		if e.Reader != nil {
			if rc, ok := e.Reader.(io.Closer); ok {
				rc.Close()
			}
		}
	}
}

// archiveTotalSize 汇总 groupFiles 源的用户可见（逻辑）大小。
func archiveTotalSize(files []fileWithRelPath) int64 {
	var total int64
	for _, f := range files {
		if sv, err := rootfs.UserVisibleSize(f.root, f.rel); err == nil {
			total += sv
		}
	}
	return total
}

// archiveTargetRel 派生转存目标在**目标卷 user 桶内**的相对路径：
//   - targetPath 空 → 默认 archive/<archive_name>；
//   - targetPath 以 "/" 结尾 → 视为目录，拼上 archiveName；
//   - 否则直接用 targetPath（去前导 "/"）。
//
// 路径安全（../ 逃逸 / 绝对路径 / 非法段）由 volume.ResolveUserLocation 唯一入口校验。
func archiveTargetRel(targetPath, archiveName string) string {
	p := strings.TrimPrefix(strings.TrimSpace(targetPath), "/")
	switch {
	case p == "":
		return relOf(archiveName)
	case strings.HasSuffix(p, "/"):
		return p + archiveName
	default:
		return p
	}
}

// relOf 派生转存目标默认路径（archive/<name>.tar.gz，卷内 user 桶相对路径；
// owner 前缀/路径安全由 volume.ResolveLocation 唯一入口拼接）。已带归档扩展名
// （`.tar.gz` / 加密产物 `.tar.gz.aes`）时不再重复追加。
func relOf(archiveName string) string {
	rel := "archive/" + archiveName
	if !strings.HasSuffix(rel, ".tar.gz") && !strings.HasSuffix(rel, ".tar.gz.aes") {
		rel += ".tar.gz"
	}
	return rel
}
