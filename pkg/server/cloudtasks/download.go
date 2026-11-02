// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// download.go 是云任务 CRUD 的 HTTP 编排入口（P1-c S2a）：创建（单条/批量）、列表、
// 详情、取消、删除、恢复。
//
// 分层纪律（docs/designs/2026-10-31-cloudtasks-deps-mapping.md §4/§9.5）：
//   - owner（任务归属与可见性）由装配层从认证上下文解析后**显式传入**；本包不持有
//     ctx→actor 的键（`ActorFrom` 的 ctx key 是装配层私有）；
//   - 响应写出用包内 writeJSON（Deps 已满 8 字段，HTTP 面在包内自洽）；
//   - 行为与迁移前逐行等价：状态码、错误文案、审计动作、批量逐项结果字段一律不变
//     （由 pkg/server 的既有 HTTP 测试继续锁定）。
package cloudtasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// Service 是云任务 CRUD 的编排入口。
//
// 除 archiveUsage（归档桶占用登记，删除时按登记释放）外无状态：全部依赖经 Deps 注入，
// 适配器逐调用读取装配层当前状态（运行期替换卷集/配置即刻生效），因此可在路由注册期
// 构造一次后并发复用。
type Service struct {
	deps  Deps
	usage *archiveUsage
}

// New 构造编排入口。Logger 为 nil 时回落 slog.Default()（与装配层响应日志的缺省一致），
// 避免响应序列化失败路径 panic。
func New(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Service{deps: deps, usage: &archiveUsage{}}
}

// 响应头/文案/审计结果常量。
//
// 文案与装配层 pkg/server/errors.go 逐字一致：既有 HTTP 测试断言 message，改写即回归。
// 审计结果的**值**与 pkg/server 的 AuditResult* 同源（审计类型/常量的唯一实现处是
// pkg/audit，此处只承载值契约，不定义审计类型）。
const (
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"

	msgInvalidRequestBody = "invalid request body"
	msgBadRequest         = "请求体校验失败"
	msgNotFound           = "not found"

	auditResultSuccess = "success"
	auditResultError   = "error"
)

// writeJSON 写出 JSON 响应：委托 internal/httpjson（全仓唯一实现），告警日志仍用本服务
// 的实例日志器。此前本包持有一份与装配层 sendJSONResponse 等价的副本，现已收敛。
func (s *Service) writeJSON(w http.ResponseWriter, response any, statusCode int) {
	httpjson.SendOpt(w, statusCode, response, httpjson.Opt{Logger: s.deps.Logger})
}

// uploadResponse 是请求体校验失败时的响应包。字段/JSON 名与装配层 UploadResponse
// 逐字一致（success / message / file_checksum），保证响应字节零变化。
type uploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

// BatchTaskResult 是批量创建任务的单项结果（HTTP 响应模型）。
// 字段顺序与 JSON 名与迁移前的 pkg/server.CloudBatchTaskResult 逐字一致。
type BatchTaskResult struct {
	ID       string `json:"id"`
	Owner    string `json:"owner,omitempty"` // 任务归属（创建者 AK / API key 名；空 = 全局兼容）
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// SaveOrDefault 解析客户端 save 参数（nil = 默认 true 保留，零回归）。
//
// 导出是因为组创建（group.go）与单条/批量必须共用同一默认语义——两处各写一遍会让
// 「save 缺省保留」出现第二份事实源。
func SaveOrDefault(s *bool) bool {
	if s == nil {
		return true
	}
	return *s
}

// IsStorageFull 判断错误是否为存储配额超限（全局账本或租户 quota.Scope）。
//
// 导出理由同 SaveOrDefault：组创建的 groupErrorStatus 在 group.go，507 映射必须与
// 单条/批量同源（两个哨兵错误都认）。
func IsStorageFull(err error) bool {
	return errors.Is(err, capacity.ErrStorageFull) || errors.Is(err, quota.ErrStorageFull)
}

// ParseOffsetLimit 解析 ?offset=&limit= 查询参数。
// 解析失败（缺失或非整数）返回默认值：offset=-1（不偏移）、limit=0（返回全部）。
//
// 导出理由：组列表（group.go）与任务列表共用同一分页语义。
func ParseOffsetLimit(r *http.Request) (offset, limit int) {
	offset = -1
	limit = 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	return offset, limit
}

// ValidateCloudDownloadURL 校验下载 URL 和可选的文件名。
// 执行 scheme 检查、可选 SSRF 防护、文件名提取和路径穿越防护。
// 返回 (cleanedURL, cleanedFilename, error)。
//
// 导出理由：组创建（group.go → normalizeGroupURLs）与单条/批量三条入口必须共用同一
// 清洗规则，否则同一内容的不同拼写会在某条路径上失去去重。
func ValidateCloudDownloadURL(rawURL, rawFilename string, allowPrivate bool) (string, string, error) {
	entry := cloudfilename.Entry{URL: rawURL, Filename: rawFilename}
	if err := cloudfilename.ValidateEntry(entry); err != nil {
		return "", "", err
	}
	// SSRF 深层防护：检查 host 不解析到内部 IP（除非 allowPrivate）
	if !allowPrivate {
		if hostErr := downloader.ValidateURLHost(rawURL); hostErr != nil {
			return "", "", fmt.Errorf("unsafe URL: %w", hostErr)
		}
	}
	fn, err := cloudfilename.ResolveFilename(entry)
	if err != nil {
		return "", "", err
	}
	parsed, _ := url.Parse(rawURL)
	return parsed.String(), fn, nil
}

// TransferACLRejection 校验转存目标卷对 owner 的 ACL（H2/R1：防跨租户覆写）。
// transfer 为 nil 或卷放行 → 返回 ""；卷未装配/ACL 拒绝 → 返回错误文案（调用方 403）。
//
// 导出理由：组创建（group.go）与单条/批量三条入口的 ACL 判定与文案必须同源。
func TransferACLRejection(vols Volumes, owner string, transfer *cloud.TransferSpec) string {
	if transfer == nil {
		return ""
	}
	if !vols.Allowed(owner, transfer.Volume) {
		return fmt.Sprintf("转存目标卷 %q 对当前用户不可用（ACL 拒绝）", transfer.Volume)
	}
	return ""
}

// checkTransferACL 是 TransferACLRejection 在编排核心内的取用形态。
func (s *Service) checkTransferACL(owner string, transfer *cloud.TransferSpec) string {
	return TransferACLRejection(s.deps.Volumes, owner, transfer)
}

// drainBody 强制消费请求体剩余部分，触发 SproxySig bodyValidator 的 EOF 哈希比对（I-3）。
// json.Decoder 读到自身需要的数据后即返回、不读到 EOF，导致哈希比对永不触发；此处兜底
// 读完整个 body——body 被篡改（哈希不匹配）时返回错误，调用方应在响应前拒绝（400）。
// 与装配层 drainAndVerifyBody 同一契约（HTTP 面属本包职责，函数体是两行 io.Copy）。
func drainBody(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

// withShareTarget 把 spx_target 参数叠加到分享 URL（已有则覆盖）。
// 设计：目标文件以 URL query 表达（而非新增任务结构字段），免改 CloudTask/去重键，
// 且不同 target 天然产生不同 URL ⇒ 云端任务去重不会把分卷 A/B 误吸收成同一任务。
func withShareTarget(rawURL, target string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	q := u.Query()
	q.Set("spx_target", target)
	u.RawQuery = q.Encode()
	return u.String()
}

// CreateDownload 处理 POST /api/cloud/download。
func (s *Service) CreateDownload(w http.ResponseWriter, r *http.Request, owner string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req struct {
		URL      string `json:"url"`
		Filename string `json:"filename,omitempty"`
		// TargetFile 是分享内目标文件名（可选；PikPak 分享时按名选文件，分卷 A/B/C
		// 各提交一次实现"分卷全套"）。非空时叠加到 URL query（spx_target 参数）
		// 传给下载器；空 = 下载器默认选最大视频（零回归）。
		TargetFile string `json:"target_file,omitempty"`
		// Transfer 是转存目标（可选）：下载完成后把产物转存到指定卷。
		Transfer *cloud.TransferSpec `json:"transfer,omitempty"`
		// Save 是否保留 cloud 桶副本（服务端化 keep-files；默认 true 零回归）。
		// false = 任务完成（含转存）后服务端自动删除 cloud 桶文件——客户端异常
		// 也不残留，清理状态记入 CleanupStatus 供审计。
		Save *bool `json:"save,omitempty"`
		// DownloadLocal 客户端是否下载本地（链式拉取 cloud 桶文件）。false = 服务端
		// 可转存后即删。创建期真空洞校验（不下载+不转存+不保留）以此消歧。
		DownloadLocal bool `json:"download_local,omitempty"`
		// IntegrityMustPass 强制源文件完整性（语义校验失败阻断任务，不放行标记 damaged）。
		IntegrityMustPass bool `json:"integrity_must_pass,omitempty"`
		// MetaExtra 是转存产物的自定义元数据（客户端传入；仅 Transfer 非空时有意义）。
		// 转存成功后与来源信息（source=transfer/source_url/etag）合并写入目标卷 meta.Extra。
		MetaExtra map[string]any `json:"meta_extra,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	cleanedURL, cleanedFilename, err := ValidateCloudDownloadURL(req.URL, req.Filename, s.deps.Tasks.AllowPrivate())
	if err != nil {
		s.writeJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	// target_file 非空 → 叠加 spx_target 到 URL（下载器按名选分享内文件）。
	// 同分享不同 target ⇒ URL 不同 ⇒ 云端任务去重（findByURL 语义一致）不会误吸收。
	if strings.TrimSpace(req.TargetFile) != "" {
		cleanedURL = withShareTarget(cleanedURL, strings.TrimSpace(req.TargetFile))
	}
	// 真空洞校验（不下载本地 + 无 transfer + save=false → 无任何产出）由 CreateTask
	// fail-closed 兜底；此处无需预检（download_local 消歧后语义完整）。
	// 转存目标卷 ACL 校验（H2/R1：防跨租户覆写）：owner 必须被目标卷 ACL 放行。
	if errMsg := s.checkTransferACL(owner, req.Transfer); errMsg != "" {
		s.writeJSON(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	// 创建任务并启动下载。提交时文件大小未知（-1），SubmitAndStart 的同步条件
	// （totalSize > 0 且 < syncThreshold）不满足，因此恒异步执行：客户端断连后
	// 服务端继续异步下载，不阻塞 handler。
	task, err := s.deps.Tasks.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, r.Context(), owner, cloud.TaskParams{Transfer: req.Transfer, DownloadLocal: req.DownloadLocal, Save: SaveOrDefault(req.Save), IntegrityMustPass: req.IntegrityMustPass, MetaExtra: req.MetaExtra})
	if err != nil {
		// 存储不足（全局账本或租户 Scope）映射 507，其余视为 400（URL 等输入问题已提前拦截）
		if IsStorageFull(err) {
			s.writeJSON(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		s.writeJSON(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	// 返回任务快照（避免并发修改 data race）
	snapshot, ok := s.deps.Tasks.SnapshotTask(task.ID, owner)
	if !ok {
		s.writeJSON(w, map[string]string{"error": "task created but not found"}, http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, snapshot, http.StatusOK)
}

// CreateBatchDownload 处理 POST /api/cloud/download/batch。
// 批量创建下载任务，始终异步执行。部分失败不中断，每项返回独立结果。
func (s *Service) CreateBatchDownload(w http.ResponseWriter, r *http.Request, owner string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req struct {
		URLs []cloudfilename.Entry `json:"urls"`
		// Transfer 批量转存目标（apply 到每个条目；逐条目可经 Entry 扩展覆盖）。
		Transfer *cloud.TransferSpec `json:"transfer,omitempty"`
		// Save 批量保存 cloud 桶副本（同单条语义；nil = 默认 true）。
		Save *bool `json:"save,omitempty"`
		// DownloadLocal 批量：客户端是否下载本地（同单条语义）。
		DownloadLocal bool `json:"download_local,omitempty"`
		// IntegrityMustPass 强制源文件完整性（同单条语义；batch 统一 apply）。
		IntegrityMustPass bool `json:"integrity_must_pass,omitempty"`
		// MetaExtra 批量转存自定义元数据（同单条语义；batch 统一 apply 到每个条目）。
		MetaExtra map[string]any `json:"meta_extra,omitempty"`
	}
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
		s.writeJSON(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per batch", maxBatch)}, http.StatusBadRequest)
		return
	}

	// 批量转存目标卷 ACL 统一校验（R1：此前 batch 路径漏检，CLI 链式主路径可跨租户转存）。
	if errMsg := s.checkTransferACL(owner, req.Transfer); errMsg != "" {
		s.writeJSON(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	results := make([]BatchTaskResult, 0, len(req.URLs))
	for _, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := ValidateCloudDownloadURL(entry.URL, entry.Filename, s.deps.Tasks.AllowPrivate())
		if err != nil {
			// 校验失败阶段：返回用户原始 URL/Filename（此时尚无规范化值）
			results = append(results, BatchTaskResult{
				URL:      entry.URL,
				Filename: entry.Filename,
				Status:   "failed",
				Error:    err.Error(),
			})
			continue
		}

		// 批量始终异步：nil context。transfer/save/download_local 批量统一 apply；
		// 真空洞（不下载+无 transfer+save=false）由 CreateTask fail-closed 拒绝。
		task, taskErr := s.deps.Tasks.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, nil, owner, cloud.TaskParams{Transfer: req.Transfer, DownloadLocal: req.DownloadLocal, Save: SaveOrDefault(req.Save), IntegrityMustPass: req.IntegrityMustPass, MetaExtra: req.MetaExtra})
		if taskErr != nil {
			results = append(results, BatchTaskResult{
				URL:      cleanedURL,
				Filename: cleanedFilename,
				Status:   "failed",
				Error:    taskErr.Error(),
			})
			continue
		}
		// 使用快照避免并发读写 data race
		snapshot, ok := s.deps.Tasks.SnapshotTask(task.ID, owner)
		if !ok {
			results = append(results, BatchTaskResult{
				URL:      cleanedURL,
				Filename: cleanedFilename,
				Status:   "failed",
				Error:    "task created but not found",
			})
			continue
		}
		// 成功项 URL 使用规范化值，与 GET /api/cloud/tasks/{id} 的详情一致
		results = append(results, BatchTaskResult{
			ID:       snapshot.ID,
			Owner:    snapshot.Owner,
			URL:      cleanedURL,
			Filename: cleanedFilename,
			Status:   snapshot.Status,
		})
	}

	s.writeJSON(w, map[string][]BatchTaskResult{"tasks": results}, http.StatusOK)
}

// ListTasks 处理 GET /api/cloud/tasks。
// 返回 {tasks, total} 容器；total 为按 status 过滤后的任务总数（不受分页影响）。
func (s *Service) ListTasks(w http.ResponseWriter, r *http.Request, owner string) {
	status := r.URL.Query().Get("status")
	offset, limit := ParseOffsetLimit(r)
	tasks, total := s.deps.Tasks.ListTasks(status, offset, limit, owner)
	s.writeJSON(w, map[string]any{"tasks": tasks, "total": total}, http.StatusOK)
}

// GetTask 处理 GET /api/cloud/tasks/{id}。
func (s *Service) GetTask(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	task, ok := s.deps.Tasks.SnapshotTask(id, owner)
	if !ok {
		s.writeJSON(w, map[string]string{"error": "task not found"}, http.StatusNotFound)
		return
	}
	s.writeJSON(w, task, http.StatusOK)
}

// CancelTask 处理 POST /api/cloud/tasks/{id}/cancel。
func (s *Service) CancelTask(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	if err := s.deps.Tasks.CancelTask(id, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		s.deps.Audit.Record(r.Context(), "cloud_cancel", "task", id, auditResultError, err.Error())
		s.writeJSON(w, map[string]string{"error": err.Error()}, status)
		return
	}
	s.deps.Audit.Record(r.Context(), "cloud_cancel", "task", id, auditResultSuccess, "")
	s.writeJSON(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// DeleteTask 处理 DELETE /api/cloud/tasks/{id}。
func (s *Service) DeleteTask(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	if err := s.deps.Tasks.DeleteTask(id, owner); err != nil {
		s.deps.Audit.Record(r.Context(), "cloud_delete", "task", id, auditResultError, err.Error())
		s.writeJSON(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
		return
	}
	s.deps.Audit.Record(r.Context(), "cloud_delete", "task", id, auditResultSuccess, "")
	// （历史坑）此处曾额外记一条 `cloud_group_delete`，object 是**任务 ID** ✗：
	// 既误导取证（任务删除被当成组删除），又让真正的组删除无痕。已移入组删除入口。
	s.writeJSON(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// ResumeTask 处理 POST /api/cloud/tasks/{id}/resume。
func (s *Service) ResumeTask(w http.ResponseWriter, r *http.Request, owner string) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 解析失败使用默认 false
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	if err := s.deps.Tasks.ResumeTask(id, req.Force, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		s.writeJSON(w, map[string]string{"error": err.Error()}, status)
		return
	}
	s.writeJSON(w, map[string]string{"status": "resumed"}, http.StatusOK)
}
