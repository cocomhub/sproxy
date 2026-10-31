// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/compressx"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/server/archive"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/transfer"
)

// CloudArchiveRequest 是 POST /api/cloud/tasks/{id}/archive 的请求体。
type CloudArchiveRequest struct {
	ArchiveName string `json:"archive_name,omitempty"`
	// TargetVolume 非空 = 归档打包后**转存到外部卷**（统一转存：归档=转存+可选预处理）。
	// 空 = 落租户 archive 桶（既有行为零回归）。
	TargetVolume string `json:"target_volume,omitempty"`
	// TargetPath 是转存目标在**目标卷 user 桶内**的相对路径（含文件名；可含子目录）。
	// 空 = 默认 `archive/<archive_name>`；以 "/" 结尾 = 目录（拼上 archive_name）。
	// 由客户端从卷 URI（`<卷名>:/<路径>`，cd/mv 同规约）归一后传入；路径安全由
	// volume.ResolveUserLocation 唯一入口校验（协议层只收 volume+path，不解析 URI）。
	TargetPath string `json:"target_path,omitempty"`
	// Encrypt = true 时打包输出流式加密（tar.<ext>.aes；密钥来自 archive.key_file）。
	// 仅与 TargetVolume 同用（未指定 target 时设此项 → 400，防静默忽略）。
	Encrypt bool `json:"encrypt,omitempty"`
	// Cipher 是加密算法选型（空 = aes-256-gcm）；与 Encrypt 同用。
	Cipher string `json:"cipher,omitempty"`
	// Compression 是压缩算法选型（空 = gzip）；gzip/zstd/brotli，非法值 400（不静默回退）。
	Compression string `json:"compression,omitempty"`
}

// CloudArchiveBatchRequest 是 POST /api/cloud/archive 的请求体。
type CloudArchiveBatchRequest struct {
	TaskIDs     []string `json:"task_ids"`
	ArchiveName string   `json:"archive_name,omitempty"`
	// TargetVolume 非空 = 批量归档打包后**转存到外部卷**（统一转存）；
	// 空 = 落租户 archive 桶（既有行为零回归）。
	TargetVolume string `json:"target_volume,omitempty"`
	// TargetPath 同 CloudArchiveRequest.TargetPath（目标卷 user 桶内相对路径）。
	TargetPath string `json:"target_path,omitempty"`
	// Encrypt / Cipher / Compression 同 CloudArchiveRequest（仅与 TargetVolume 同用）。
	Encrypt     bool   `json:"encrypt,omitempty"`
	Cipher      string `json:"cipher,omitempty"`
	Compression string `json:"compression,omitempty"`
}

// CloudArchiveResult 是归档操作响应结构体。
type CloudArchiveResult struct {
	Success      bool     `json:"success"`
	Message      string   `json:"message,omitempty"`
	File         string   `json:"file,omitempty"`
	Size         int64    `json:"size,omitempty"`
	Checksum     string   `json:"checksum,omitempty"`
	TaskCount    int      `json:"task_count,omitempty"`
	SkippedCount int      `json:"skipped_count,omitempty"`
	SkippedTasks []string `json:"skipped_tasks,omitempty"`
}

// cloudArchiveReservePlaceholder 云归档打包预留占位（100MB）。
// tar 头（≤512B/文件）+ gzip 可能在极少数情况下产生少量膨胀，预留后按实际大小对账。
const cloudArchiveReservePlaceholder = int64(100 * 1024 * 1024)

// cloudArchiveMaxBytes 返回云归档总量上限（0 = 不限制，仍受 max_storage_bytes 兜底）。
func (h *Handlers) cloudArchiveMaxBytes() int64 {
	if cfg := h.cfgPtr.Load(); cfg != nil {
		return cfg.CloudArchiveMaxBytes
	}
	return 0
}

// cloudArchiveTask 处理 POST /api/cloud/tasks/{id}/archive。
// 将已完成云下载任务的文件打包为 tar.gz 归档文件。
//
// 排除面边界（F5 review 成文）：cloud 源（cloud 桶）与 archive 输出（archive 桶）均落**默认卷**
// （tenantFor），属 §11「cloud/archive 产物落默认卷」设计内例外——默认卷被 ACL 排除的 owner 仍可
// 经自建归档回读默认卷上自己生成的 cloud 内容。见 volumes.go defaultVolumeAllows 边界注释。
// 不改行为，仅记录避免未来误判为漏洞。
func (h *Handlers) cloudArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	// 校验任务存在且状态为 completed、解析请求体、推导任务源路径（失败已回响应）。
	task, req, srcTnt, srcRel, ok := h.cloudArchivePrepare(w, r, taskID)
	if !ok {
		return
	}

	// 确定归档文件名（路径穿越防护 + 合法性检查）
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-task-%s-%d.tar.gz", taskID, time.Now().Unix())
	}

	// 打包前：单文件总量限制（cloud_archive_max_bytes）。单文件仍受 addFileToTar 内
	// archive.DefaultMaxArchiveSize=100MB 约束；此处限制的是原始文件大小，与 addFileToTar 并存不冲突。
	// 审查 #10 结论（勿再分析）：此 os.Stat 是**必须的**大小校验，并非与 O_EXCL 重复的
	// 存在性预检——同名冲突由 openArchiveOutput 的 O_EXCL 单一负责（createTarGz 返回
	// errArchiveExists → 409）；此处 stat 失败即源文件不存在（400），职责不同，无冗余。
	info, err := srcTnt.Root().Stat(srcRel)
	if err != nil {
		h.logger.Error("failed to stat source file in cloud archive", "task_id", taskID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return
	}

	// 统一转存：target_volume → 打包转存外部卷（统一「归档 = 转存 + 预处理」管道），
	// 不落租户 archive 桶。target_path/encrypt/cipher/compression 仅与 target_volume 同用。
	if !h.validateArchiveExtras(w, archiveExtras{
		TargetVolume: req.TargetVolume, TargetPath: req.TargetPath,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
	}) {
		return
	}
	if req.TargetVolume != "" {
		h.cloudArchiveTaskToVolume(w, r, taskID, task, req, srcTnt, srcRel, info, archiveName)
		return
	}

	// 确保输出目录存在 + 推导输出 rel（失败已回响应）。
	owner, root, rel, archiveName, ok := h.cloudArchiveOutput(w, r, archiveName)
	if !ok {
		return
	}

	// 总量限制 + 预留配额（scope 优先，storageMgr 回退；与批量/组归档同款逻辑）。
	res, pre, ok := h.cloudArchiveReserveQuota(r.Context(), w, owner, info.Size())
	if !ok {
		return
	}

	// 打包（流式 checksum）
	logger := h.logger.With("archive", "cloud_task", "task_id", taskID)
	checksum, err := createTarGz(srcTnt.Root(), srcRel, task.Filename, root, rel, logger)
	if err != nil {
		if errors.Is(err, errArchiveExists) {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			releaseArchiveReservation(res, h.storageMgr, pre)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		h.logger.Error("failed to create archive", "task_id", taskID, "error", err)
		_ = root.Remove(rel)
		releaseArchiveReservation(res, h.storageMgr, pre)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际对账预留配额（scope 优先 Commit(actual)，storageMgr 回退 3 步；失败已回 507）。
	actual, settleOK := h.cloudArchiveSettleQuota(w, root, rel, res, pre, h.logger.With("task_id", taskID))
	if !settleOK {
		return
	}

	// P5 归档占用登记（登记后释放 Scope 不再依赖周期扫描自愈）。
	h.recordArchiveUsage(owner, archiveName, actual)

	archiveInfo, err := root.Stat(rel)
	if err != nil {
		h.logger.Error("failed to stat archive", "task_id", taskID, "error", err)
	}

	archiveSize := int64(0)
	if archiveInfo != nil {
		archiveSize = archiveInfo.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:   true,
		File:      archiveName,
		Size:      archiveSize,
		Checksum:  checksum,
		TaskCount: 1,
	}, http.StatusOK)
}

// cloudArchiveBatch 处理 POST /api/cloud/archive。
// 批量将多个已完成云下载任务的文件打包为单个 tar.gz 归档文件。
func (h *Handlers) cloudArchiveBatch(w http.ResponseWriter, r *http.Request) {
	// 解析请求体 + 校验 task_ids 数量（失败已回响应）。
	req, ok := h.cloudArchiveBatchParse(w, r)
	if !ok {
		return
	}

	// 收集所有已完成任务的文件信息，跳过无效任务（全部跳过已回响应）。
	files, skippedTasks, totalSourceSize, ok := h.cloudArchiveCollectTasks(w, r, req.TaskIDs)
	if !ok {
		return
	}

	// 确定归档文件名（路径穿越防护 + 合法性检查）
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-batch-%d.tar.gz", time.Now().Unix())
	}

	// 统一转存：target_volume → 打包转存外部卷（统一「归档 = 转存 + 预处理」管道），
	// 不落租户 archive 桶。target_path/encrypt/cipher/compression 仅与 target_volume 同用。
	if !h.validateArchiveExtras(w, archiveExtras{
		TargetVolume: req.TargetVolume, TargetPath: req.TargetPath,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
	}) {
		return
	}
	if req.TargetVolume != "" {
		h.cloudArchiveBatchToVolume(w, r, req, files, totalSourceSize, archiveName)
		return
	}

	// 确保输出目录存在 + 推导输出 rel（失败已回响应）。
	owner, root, rel, archiveName, ok := h.cloudArchiveOutput(w, r, archiveName)
	if !ok {
		return
	}

	// 打包前：总量限制 + 预留配额（scope 优先，storageMgr 回退；与单任务/组归档一致）。
	res, pre, ok := h.cloudArchiveReserveQuota(r.Context(), w, owner, totalSourceSize)
	if !ok {
		return
	}

	// 多文件打包（流式 checksum）
	created := false
	logger := h.logger.With("archive", "cloud_batch")
	checksum, err := createMultiFileTarGz(files, root, rel, logger, &created)
	if err != nil {
		if !created {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			releaseArchiveReservation(res, h.storageMgr, pre)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		h.logger.Error("failed to create batch archive", "error", err)
		_ = root.Remove(rel)
		releaseArchiveReservation(res, h.storageMgr, pre)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际对账预留配额（TryReserve 拒绝已回 507）。
	actual, settleOK := h.cloudArchiveSettleQuota(w, root, rel, res, pre, h.logger)
	if !settleOK {
		return
	}

	// P5 归档占用登记（删除组时按登记 Release Scope）。
	h.recordArchiveUsage(owner, archiveName, actual)

	info, err := root.Stat(rel)
	if err != nil {
		h.logger.Error("failed to stat archive", "error", err)
	}

	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:      true,
		File:         archiveName,
		Size:         size,
		Checksum:     checksum,
		TaskCount:    len(files),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// cloudArchivePrepare 校验云任务（存在 + completed + 归属请求者）、解析请求体、
// 推导任务源文件路径（任务 owner 租户 cloud 桶）。任一失败已写响应并返回 ok=false。
func (h *Handlers) cloudArchivePrepare(w http.ResponseWriter, r *http.Request, taskID string) (task *cloud.CloudTask, req CloudArchiveRequest, srcTnt *storage.Tenant, srcRel string, ok bool) {
	// 校验任务存在且状态为 completed（使用 SnapshotTask 避免 data race）。
	// 按请求者 owner 过滤：跨 owner 任务视为不存在（404 防枚举）。
	var found bool
	task, found = h.cloudMgr.SnapshotTask(taskID, ActorFrom(r.Context()))
	if !found {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task not found"}, http.StatusNotFound)
		return task, req, nil, "", false
	}
	if task.Status != "completed" {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false,
			Message: fmt.Sprintf("task status is %q, expected \"completed\"", task.Status),
		}, http.StatusBadRequest)
		return task, req, nil, "", false
	}

	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return task, req, nil, "", false
	}

	// 构造源文件路径（任务文件按任务 owner 落租户 cloud/ 桶）：根内 rel = cloud/<taskID>/<file>
	// 经 FeatureRel 校验段名（fail-closed，防 task.Filename 穿越任务目录）。
	srcTnt = h.tenantFor(task.Owner)
	if srcTnt == nil {
		h.logger.Error("云任务源租户不可用（fail-closed）", "task_id", taskID, "owner", task.Owner)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	var srcOK bool
	srcRel, srcOK = srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
	if !srcOK {
		h.logger.Error("云任务源路径非法（fail-closed）", "task_id", taskID, "file", task.Filename)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	return task, req, srcTnt, srcRel, true
}

// cloudArchiveOutput 解析归档输出路径：防穿越校验归档名（加 .tar.gz 后缀、限长）、
// 确保租户 archive 桶目录存在并推导输出 rel。失败时已写响应并返回 ok=false。
func (h *Handlers) cloudArchiveOutput(w http.ResponseWriter, r *http.Request, archiveName string) (owner string, root *storage.Root, rel, name string, ok bool) {
	// 路径穿越防护 + 后缀/长度校验（与统一转存共用同一校验）。
	archiveName, ok = h.normalizeArchiveName(w, archiveName)
	if !ok {
		return "", nil, "", "", false
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<owner>/archive/）
	owner = ActorFrom(r.Context())
	tnt := h.tenantFor(owner)
	if tnt == nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return "", nil, "", "", false
	}
	root = tnt.Root()
	if err := root.MkdirAll("archive", 0755); err != nil {
		h.logger.Error(msgArchiveDirFail, "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return "", nil, "", "", false
	}
	var relOK bool
	rel, relOK = tnt.FeatureRel("archive", archiveName)
	if !relOK {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return "", nil, "", "", false
	}
	return owner, root, rel, archiveName, true
}

// normalizeArchiveName 校验并归一归档名：仅文件名（Base，防 ../ 与 /）、补 .tar.gz
// 后缀、长度 ≤255。失败时已写响应并返回 ok=false。bucket 落盘与统一转存共用，防
// 两套校验漂移。
func (h *Handlers) normalizeArchiveName(w http.ResponseWriter, archiveName string) (string, bool) {
	// 路径穿越防护：仅允许文件名，拒绝 ../ 和 /
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive name"}, http.StatusBadRequest)
		return "", false
	}
	// 确保以 .tar.gz 结尾
	if !strings.HasSuffix(archiveName, tarGZExt) {
		archiveName += tarGZExt
	}
	// 长度限制
	if len(archiveName) > 255 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive name too long"}, http.StatusBadRequest)
		return "", false
	}
	return archiveName, true
}

// cloudArchiveTaskToVolume 单任务归档统一转存：单文件（tar.gz 打包）→ 外部卷。
// ACL 与组/批量归档同语义（volumeAllowedFor 防跨租户写私有卷）。
func (h *Handlers) cloudArchiveTaskToVolume(w http.ResponseWriter, r *http.Request, taskID string, task *cloud.CloudTask, req CloudArchiveRequest, srcTnt *storage.Tenant, srcRel string, info os.FileInfo, archiveName string) {
	owner := ActorFrom(r.Context())
	if !h.volumeAllowedFor(owner, req.TargetVolume) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
		return
	}
	name, ok := h.normalizeArchiveName(w, archiveName)
	if !ok {
		return
	}
	// R25：用户可见读入口（加密卷透明解密）——不得直调 root.Open。
	f, oerr := rootfs.OpenUserVisibleRead(srcTnt.Root(), srcRel)
	if oerr != nil {
		h.logger.Error("统一转存打开源文件失败", "task_id", taskID, "error", oerr)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "open source failed: " + oerr.Error()}, http.StatusInternalServerError)
		return
	}
	// 用户可见（逻辑）长度：加密卷取明文长度，否则等同 Stat。
	size := info.Size()
	if sv, vErr := rootfs.UserVisibleSize(srcTnt.Root(), srcRel); vErr == nil {
		size = sv
	}
	entries := []transfer.SourceEntry{{
		Rel: task.Filename, Size: size, ModTime: info.ModTime().Unix(), Reader: f,
	}}
	h.cloudArchiveToVolume(w, r, archiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: name,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: size,
	}, h.logger.With("archive", "cloud_task", "task_id", taskID), nil)
}

// cloudArchiveBatchToVolume 批量归档统一转存：多文件（tar.gz 打包）→ 外部卷。
func (h *Handlers) cloudArchiveBatchToVolume(w http.ResponseWriter, r *http.Request, req CloudArchiveBatchRequest, files []fileWithRelPath, totalSize int64, archiveName string) {
	owner := ActorFrom(r.Context())
	if !h.volumeAllowedFor(owner, req.TargetVolume) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
		return
	}
	name, ok := h.normalizeArchiveName(w, archiveName)
	if !ok {
		return
	}
	entries, err := archiveEntries(files)
	if err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "open source failed: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	h.cloudArchiveToVolume(w, r, archiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: name,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: totalSize,
	}, h.logger.With("archive", "cloud_batch"), nil)
}

// cloudArchiveSettleQuota 按磁盘实际大小对账预留配额（单任务 / 批量 / 组归档共用）：scope 路径
// = Reconcile(actual) + Commit()（多预留部分自动归还）；storageMgr 回退保留 3 步
// （Release(pre)+TryReserve(actual)）。TryReserve 拒绝时已删文件并回 507，返回 ok=false。
func (h *Handlers) cloudArchiveSettleQuota(w http.ResponseWriter, root *storage.Root, rel string, res *quota.Settlement, pre int64, logger *slog.Logger) (actual int64, ok bool) {
	if res != nil {
		info, statErr := root.Stat(rel)
		if statErr != nil {
			// stat 失败（罕见）：释放预留避免永久挂账（归档文件已落盘，下次扫描校准全局账本）。
			res.Release()
			return 0, true
		}
		actual = info.Size()
		commitArchiveSettlement(res, pre, actual, logger)
		return actual, true
	}
	if h.storageMgr == nil {
		return 0, true
	}
	h.storageMgr.Release(pre, capacity.CategoryCloud)
	info, statErr := root.Stat(rel)
	if statErr != nil {
		return 0, true
	}
	actual = info.Size()
	if rErr := h.storageMgr.TryReserve(actual, capacity.CategoryCloud); rErr != nil {
		logger.Error("storage full, removing archive to keep ledger consistent", "error", rErr)
		_ = root.Remove(rel)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("insufficient storage for archive: %v", rErr),
		}, http.StatusInsufficientStorage)
		return 0, false
	}
	return actual, true
}

// cloudArchiveBatchParse 解析批量归档请求体并校验 task_ids 数量（1..100）。
// 失败已写响应并返回 ok=false。
func (h *Handlers) cloudArchiveBatchParse(w http.ResponseWriter, r *http.Request) (req CloudArchiveBatchRequest, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return req, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return req, false
	}

	if len(req.TaskIDs) == 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task_ids is required"}, http.StatusBadRequest)
		return req, false
	}
	if len(req.TaskIDs) > 100 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "maximum 100 task IDs per request"}, http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// cloudArchiveCollectTasks 逐任务收集已完成任务的文件信息（无效任务跳过并记 skipped）。
// 所有任务都被跳过时已写响应并返回 ok=false。
func (h *Handlers) cloudArchiveCollectTasks(w http.ResponseWriter, r *http.Request, taskIDs []string) (files []fileWithRelPath, skippedTasks []string, totalSourceSize int64, ok bool) {
	for _, taskID := range taskIDs {
		f, size, skip := h.cloudArchiveTaskSource(r, taskID)
		if skip {
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		totalSourceSize += size
		files = append(files, f)
	}

	// 所有任务都被跳过则返回错误
	if len(files) == 0 {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "no valid tasks to archive",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return nil, skippedTasks, totalSourceSize, false
	}
	return files, skippedTasks, totalSourceSize, true
}

// cloudArchiveTaskSource 解析批量归档单个任务的文件信息（快照 + completed + 租户/路径/
// 存在性校验）。解析失败记 Warn 日志并返回 skip=true（调用方累积 skipped 列表）。
func (h *Handlers) cloudArchiveTaskSource(r *http.Request, taskID string) (fileWithRelPath, int64, bool) {
	// 使用 SnapshotTask 避免 data race；按请求者 owner 过滤（跨 owner 任务跳过，防内容外泄）。
	task, found := h.cloudMgr.SnapshotTask(taskID, ActorFrom(r.Context()))
	if !found {
		h.logger.Warn("cloud batch archive: skipping task not found", "task_id", taskID)
		return fileWithRelPath{}, 0, true
	}
	if task.Status != "completed" {
		h.logger.Warn("cloud batch archive: skipping task with non-completed status",
			"task_id", taskID, "status", task.Status)
		return fileWithRelPath{}, 0, true
	}

	// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）
	srcTnt := h.tenantFor(task.Owner)
	if srcTnt == nil {
		h.logger.Warn("cloud batch archive: skipping task with unavailable tenant",
			"task_id", taskID, "owner", task.Owner)
		return fileWithRelPath{}, 0, true
	}
	srcRel, relOK := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
	if !relOK {
		h.logger.Warn("cloud batch archive: skipping task with invalid source path",
			"task_id", taskID, "file", task.Filename)
		return fileWithRelPath{}, 0, true
	}

	// 顺带 stat 校验文件存在并累计原始大小（用于总量限制与配额预估）
	info, statErr := srcTnt.Root().Stat(srcRel)
	if statErr != nil {
		h.logger.Warn("cloud batch archive: skipping missing file",
			"task_id", taskID, "rel", srcRel, "error", statErr)
		return fileWithRelPath{}, 0, true
	}

	relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
	return fileWithRelPath{root: srcTnt.Root(), rel: srcRel, tarRel: relPath}, info.Size(), false
}

type fileWithRelPath struct {
	root   *storage.Root
	rel    string // 根内相对源路径（cloud/<taskID>/<file>）
	tarRel string // tar 内条目名
}

// createTarGz 将单个文件打包为 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在返回 false 到 created。
// 源文件经 srcRoot 相对 srcRel 读取、输出经 outRoot 相对 outRel 打开（os.Root 防符号链接逃逸；O_EXCL 语义保留）。
func createTarGz(srcRoot *storage.Root, srcRel, srcTarName string, outRoot *storage.Root, outRel string, logger *slog.Logger) (checksum string, err error) {
	outputFile, created, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !created {
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// **收尾顺序必须显式**（P1 修复，2026-10-21）：此前 tw/gw 都是 `defer Close()`，而
	// checksum 在 defer 之前就计算 ⇒ ① 哈希漏掉 tar 的 EOF 两块 + gzip 尾（CRC32/ISIZE）
	// ⇒ 响应里的 checksum 100% 不等于落盘文件（客户端按它校验必然判「归档损坏」）；
	// ② Close 的错误被丢弃且 succeeded 已置 true ⇒ 收尾失败的**截断归档**被当成功返回
	// 且不清理。与 pkg/server/archive.go、pkg/transfer/registry.go 的既有口径统一。
	if err := archive.AddFileToTar(tw, srcRoot, srcRel, srcTarName, logger); err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return "", fmt.Errorf("add file to tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// createMultiFileTarGz 将多个文件打包为单个 tar.gz 并返回流式计算的 SHA-256 checksum。
// 使用 succeeded 标记模式确保出错时清理输出文件。O_EXCL 创建，已存在时置 created=false 并返回 errArchiveExists。
// 源文件按各自租户根（files[].root）相对读取、输出经 outRoot 相对 outRel 打开
// （os.Root 防符号链接逃逸；O_EXCL 语义保留）。
func createMultiFileTarGz(files []fileWithRelPath, outRoot *storage.Root, outRel string, logger *slog.Logger, created *bool) (checksum string, err error) {
	*created = true
	outputFile, createdOK, err := openArchiveOutput(outRoot, outRel)
	if err != nil {
		return "", err
	}
	if !createdOK {
		*created = false
		return "", errArchiveExists
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			_ = outRoot.Remove(outRel)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outputFile, hasher)
	gw := gzip.NewWriter(multiWriter)
	tw := tar.NewWriter(gw)

	// 与 createTarGz 同口径（P1 修复，2026-10-21）：显式收尾 + 收尾后再算 checksum。
	for _, f := range files {
		if err := archive.AddFileToTar(tw, f.root, f.rel, f.tarRel, logger); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			return "", fmt.Errorf("add file %q to tar: %w", f.tarRel, err)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return "", fmt.Errorf("关闭 tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("关闭 gzip: %w", err)
	}
	if err := outputFile.Close(); err != nil {
		return "", fmt.Errorf("关闭归档文件: %w", err)
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// errArchiveExists 归档输出文件已存在（O_EXCL 创建失败）。
var errArchiveExists = errors.New("archive file already exists")

// openArchiveOutput 以 O_EXCL 语义打开归档输出文件（相对租户 root 的 rel 路径）。
// 跨平台统一用 errors.Is(err, os.ErrExist) 判已存在（Windows 上 O_EXCL 对已存在文件同样报错）。
func openArchiveOutput(root *storage.Root, rel string) (io.WriteCloser, bool, error) {
	// 归档输出是**用户内容**写面：加密卷须透明加密落盘（raw OpenFile 写明文，
	// 归档产物读回时按密文形态不完整；评审「用户可见入口」收敛面）。
	f, err := rootfs.WriteUserVisible(root, rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("create archive output: %w", err)
	}
	return f, true, nil
}

// recordArchiveUsage 登记归档文件在租户 archive 桶 Scope 中的已确认占用
// （P5 审查重要 2：删除归档时按登记释放，不再依赖周期扫描自愈）。
// actual<=0 时不登记（文件未落盘，无占用可释放）。
func (h *Handlers) recordArchiveUsage(owner, archiveName string, actual int64) {
	if actual <= 0 {
		return
	}
	owner = normalizeOwner(owner)
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	if h.archiveUsage == nil {
		h.archiveUsage = make(map[string]map[string]int64)
	}
	m := h.archiveUsage[owner]
	if m == nil {
		m = make(map[string]int64)
		h.archiveUsage[owner] = m
	}
	m[archiveName] = actual
}

// deleteCloudArchive 删除租户 archive 桶下指定归档文件并释放其 Scope 占用。
// 供归档删除路径（DeleteGroup 联动 / 未来归档删除 API）调用；文件不存在视为幂等成功。
// 释放量以磁盘 stat 为准（与登记一致；登记缺失时按 stat 释放，防止 Scope 虚高）。
func (h *Handlers) deleteCloudArchive(owner, archiveName string) error {
	owner = normalizeOwner(owner)
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return errors.New("租户不可用，无法删除归档")
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		return fmt.Errorf("非法归档名: %s", archiveName)
	}
	root := tnt.Root()
	size := int64(0)
	if info, err := root.Stat(rel); err == nil {
		size = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat 归档失败: %w", err)
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除归档失败: %w", err)
	}
	// 释放 archive 桶 Scope 占用（按磁盘 stat 大小；文件已不存在时按登记释放）。
	if size <= 0 {
		h.tenantMu.Lock()
		if m := h.archiveUsage[owner]; m != nil {
			size = m[archiveName]
		}
		h.tenantMu.Unlock()
	}
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil && size > 0 {
		scope.ReleaseUsage(size)
	}
	h.tenantMu.Lock()
	if m := h.archiveUsage[owner]; m != nil {
		delete(m, archiveName)
	}
	h.tenantMu.Unlock()
	h.logger.Info("云归档已删除并释放配额", "owner", owner, "archive", archiveName, "size", size)
	return nil
}

// archiveExtras 是「仅转存外部卷支持」的选项集合（校验入参收拢为结构体，避免长参数列表）。
type archiveExtras struct {
	TargetVolume string
	TargetPath   string
	Encrypt      bool
	Cipher       string
	Compression  string
}

// validateArchiveExtras 校验「仅转存外部卷支持」的选项：
//   - 未指定 target_volume 时不得设置 target_path/encrypt/cipher/compression
//     （archive 桶路径走既有 createTarGz，不加密也不认目标路径）；
//   - cipher 必须与 encrypt 同用（单独指定无任何加密效果，静默忽略会让用户误信已加密）。
//
// 失败时已写响应并返回 false。
func (h *Handlers) validateArchiveExtras(w http.ResponseWriter, ex archiveExtras) bool {
	if ex.TargetVolume == "" {
		if ex.Encrypt || ex.Cipher != "" || ex.Compression != "" || ex.TargetPath != "" {
			return h.archiveBadRequest(w, "target_path/encrypt/cipher/compression 仅支持与 target_volume（转存外部卷）同用")
		}
		return true
	}
	if ex.Cipher != "" && !ex.Encrypt {
		return h.archiveBadRequest(w, "cipher 仅与 encrypt 同用（单独指定不加密，已拒绝以避免误信已加密）")
	}
	return true
}

// archiveBadRequest 回 400 并返回 false（统一归档端点的参数错误应答）。
func (h *Handlers) archiveBadRequest(w http.ResponseWriter, msg string) bool {
	sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
	return false
}

// archivePreprocessor 按请求构造打包预处理器（压缩 + 可选加密，经 transfer registry 选型）。
// 加密密钥来自 archive.key_file（与 /api/archive 同一约定）。失败已回响应。
func (h *Handlers) archivePreprocessor(w http.ResponseWriter, encrypt bool, cipher, compression string) (transfer.Preprocessor, bool) {
	algo := compressx.Gzip
	if compression != "" {
		parsed, err := compressx.Parse(compression)
		if err != nil {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusBadRequest)
			return nil, false
		}
		algo = parsed
	}
	cfg := transfer.PreprocessorConfig{Compression: algo, Cipher: cipher}
	if encrypt {
		if cipher != "" {
			if _, ok := files.LookupCipher(cipher); !ok {
				sendJSONResponse(w, CloudArchiveResult{Success: false, Message: fmt.Sprintf("未知加密算法 %q", cipher)}, http.StatusBadRequest)
				return nil, false
			}
		}
		key := h.archiveKey()
		if len(key) != 32 {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "归档加密密钥不可用（需配置 archive.key_file）"}, http.StatusBadRequest)
			return nil, false
		}
		cfg.EncryptKey = key
	}
	pre, err := transfer.New("tar", cfg)
	if err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: err.Error()}, http.StatusInternalServerError)
		return nil, false
	}
	return pre, true
}
