// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// group.go 是云任务组（group）的 HTTP 编排入口（P1-c S2b）：创建、列表、详情、
// 取消、删除、恢复。
//
// 分层纪律与 download.go 相同（docs/designs/2026-10-31-cloudtasks-deps-mapping.md
// §4/§9.5）：owner 由装配层解析后显式传入；响应写出用包内 writeJSON；行为与迁移前
// 逐行等价（状态码、错误文案、审计动作一律不变，由 pkg/server 的既有 HTTP 测试锁定）。
//
// 组归档产物的清理（deleteGroupArchive）**仍在装配层**：归档自身的读写在 S3 才迁入，
// 本片只经 Groups.DeleteGroupArchive 需要它的删除能力（S3 迁入归档面后移除该委托）。
package cloudtasks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// msgGroupNotFound 与装配层 pkg/server/errors.go 的同名文案逐字一致（既有测试断言 message）。
const msgGroupNotFound = "group not found"

// cloudGroupCreateRequest 是组创建请求体（字段顺序与 JSON 名与迁移前逐字一致）。
type cloudGroupCreateRequest struct {
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

// CreateGroup 处理 POST /api/cloud/groups。
func (s *Service) CreateGroup(w http.ResponseWriter, r *http.Request, owner string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req cloudGroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		s.writeJSON(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := s.deps.Tasks.MaxBatchURLs(); len(req.URLs) > maxBatch {
		s.writeJSON(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per group", maxBatch)}, http.StatusBadRequest)
		return
	}
	// 转存目标卷 ACL 校验（H2/R1：防跨租户覆写）：owner 必须被目标卷 ACL 放行。
	// 与单条/批量路径一致（组内每个子任务转存同目标卷）。
	if errMsg := s.checkTransferACL(owner, req.Transfer); errMsg != "" {
		s.writeJSON(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	// 校验并规范化 URL：必须把规范化后的 URL/Filename 传给 CreateGroup，与单条/
	// 批量路径保持一致——否则同一内容的不同拼写（如 http://host/a 与 http://host/a/）
	// 在单条路径会被去重、在组路径会生成两个下载，组内文件名冲突判定也基于未
	// 规范化的值，导致与 UI/CLI 本地预检偶发不一致。
	normalized, nerr := normalizeGroupURLs(req.URLs, s.deps.Tasks.AllowPrivate())
	if nerr != nil {
		s.writeJSON(w, map[string]string{"error": nerr.Error()}, http.StatusBadRequest)
		return
	}

	group, err := s.deps.Groups.SubmitAndStartGroup(req.Name, normalized, owner, cloud.TaskParams{Transfer: req.Transfer, DownloadLocal: req.DownloadLocal, Save: SaveOrDefault(req.Save), IntegrityMustPass: req.IntegrityMustPass})
	if err != nil {
		s.writeJSON(w, map[string]string{"error": err.Error()}, groupErrorStatus(err))
		return
	}
	// 返回快照副本：SubmitAndStartGroup 返回的指针与 m.groups 共享，下载 goroutine
	// 可能在 json.Marshal 期间并发写组状态字段（UpdateGroupStatus），需副本隔离防 data race。
	s.deps.Audit.Record(r.Context(), "cloud_group_create", "task", group.ID, auditResultSuccess,
		"urls="+fmt.Sprintf("%d", len(normalized)))
	snapshot, ok := s.deps.Groups.GetGroup(group.ID, owner)
	if !ok {
		s.writeJSON(w, map[string]string{"error": "group created but not found"}, http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, snapshot, http.StatusOK)
}

// normalizeGroupURLs 校验并规范化组内每个 URL（单条/批量同款规则），返回规范化条目或错误。
// 抽离 CreateGroup 控制 gocognit（gocognit=15 门禁）。
func normalizeGroupURLs(urls []cloudfilename.Entry, allowPrivate bool) ([]cloudfilename.Entry, error) {
	normalized := make([]cloudfilename.Entry, len(urls))
	for i, entry := range urls {
		cleanedURL, cleanedFilename, err := ValidateCloudDownloadURL(entry.URL, entry.Filename, allowPrivate)
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
	if IsStorageFull(err) {
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

// GetGroup 处理 GET /api/cloud/groups/{id}。
func (s *Service) GetGroup(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")

	// 审查 Minor 2：先 GetGroup 校验 owner 可见性（跨 owner 立即 404，不对不可见资源
	// 执行 UpdateGroupStatus 写操作，消除计时侧信道），通过后再刷新状态并二次 GetGroup
	// 取最新快照。
	if _, ok := s.deps.Groups.GetGroup(id, owner); !ok {
		s.writeJSON(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}
	s.deps.Groups.UpdateGroupStatus(id)
	group, ok := s.deps.Groups.GetGroup(id, owner)
	if !ok {
		s.writeJSON(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 获取组详情时一并返回子任务（仅对请求者可见的子任务，IDOR 防护）。
	// 批量快照走领域 API（domain 侧一次持锁 + 同一 ownerVisible 规则），装配层不触碰
	// 领域内部状态。
	tasks := s.deps.Tasks.SnapshotTasks(group.TaskIDs, owner)

	s.writeJSON(w, map[string]any{
		"group": group,
		"tasks": tasks,
	}, http.StatusOK)
}

// ListGroups 处理 GET /api/cloud/groups。
// 返回 {groups, total} 容器；total 为按 status 过滤后的组总数（不受分页影响）。
func (s *Service) ListGroups(w http.ResponseWriter, r *http.Request, owner string) {
	status := r.URL.Query().Get("status")
	offset, limit := ParseOffsetLimit(r)
	// 先刷新所有可见组的最新状态，再按 status 过滤返回。否则只刷新"当前已处于该状态"
	// 的组，刚转换到目标状态的组会被过滤查询漏掉，客户端看到的状态滞后。
	allGroups, _ := s.deps.Groups.ListGroups("", -1, 0, owner)
	for _, g := range allGroups {
		s.deps.Groups.UpdateGroupStatus(g.ID)
	}
	// total 需按同 status 过滤后的总数计算（ListGroups 内部过滤后统计）
	groups, total := s.deps.Groups.ListGroups(status, offset, limit, owner)
	s.writeJSON(w, map[string]any{"groups": groups, "total": total}, http.StatusOK)
}

// CancelGroup 处理 POST /api/cloud/groups/{id}/cancel。
func (s *Service) CancelGroup(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	if err := s.deps.Groups.CancelGroup(id, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		s.writeJSON(w, map[string]string{"error": err.Error()}, status)
		return
	}
	s.writeJSON(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// DeleteGroup 处理 DELETE /api/cloud/groups/{id}。
// P5：组删除时联动清理组归档文件并释放 archive 桶 Scope（防孤儿归档虚高占用）。
func (s *Service) DeleteGroup(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	// 先取组快照（含 ArchiveFile），DeleteGroup 后组对象已从 map 移除无法再查。
	var archiveFile string
	if g, ok := s.deps.Groups.GetGroup(id, owner); ok {
		archiveFile = g.ArchiveFile
	}
	if err := s.deps.Groups.DeleteGroup(id, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		s.deps.Audit.Record(r.Context(), "cloud_group_delete", "task", id, auditResultError, err.Error())
		s.writeJSON(w, map[string]string{"error": err.Error()}, status)
		return
	}
	s.deps.Audit.Record(r.Context(), "cloud_group_delete", "task", id, auditResultSuccess, "")
	// 组归档文件联动删除（桶归档名或外部卷转存 URL）。失败仅记日志
	// （组删除已完成，归档残留由周期扫描校准兜底；不把归档清理失败当作组删除失败）。
	if archiveFile != "" {
		if aErr := s.deps.Groups.DeleteGroupArchive(owner, archiveFile); aErr != nil {
			s.deps.Logger.Warn("删除组归档失败（残留由周期扫描校准）", "group_id", id, "archive", archiveFile, "error", aErr)
		}
	}
	s.writeJSON(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// ResumeGroup 处理 POST /api/cloud/groups/{id}/resume。
func (s *Service) ResumeGroup(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	if err := s.deps.Groups.ResumeGroup(id, req.Force, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		s.writeJSON(w, map[string]string{"error": err.Error()}, status)
		return
	}
	s.writeJSON(w, map[string]string{"status": "resumed"}, http.StatusOK)
}
