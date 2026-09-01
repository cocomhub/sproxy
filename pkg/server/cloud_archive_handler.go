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

	"github.com/cocomhub/sproxy/pkg/storage"
)

// CloudArchiveRequest 是 POST /api/cloud/tasks/{id}/archive 的请求体。
type CloudArchiveRequest struct {
	ArchiveName string `json:"archive_name,omitempty"`
}

// CloudArchiveBatchRequest 是 POST /api/cloud/archive 的请求体。
type CloudArchiveBatchRequest struct {
	TaskIDs     []string `json:"task_ids"`
	ArchiveName string   `json:"archive_name,omitempty"`
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

// cloudArchiveDirName 是云任务归档文件存储子目录。
const cloudArchiveDirName = ".__cloud_archives__"

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
func (h *Handlers) cloudArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	// 校验任务存在且状态为 completed（使用 SnapshotTask 避免 data race）。
	// 按请求者 owner 过滤：跨 owner 任务视为不存在（404 防枚举）。
	task, ok := h.cloudMgr.SnapshotTask(taskID, ActorFrom(r.Context()))
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task not found"}, http.StatusNotFound)
		return
	}
	if task.Status != "completed" {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false,
			Message: fmt.Sprintf("task status is %q, expected \"completed\"", task.Status),
		}, http.StatusBadRequest)
		return
	}

	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req CloudArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	// 构造源文件路径（任务文件按任务 owner 落租户 cloud/ 桶）：根内 rel = cloud/<taskID>/<file>
	// 经 FeatureRel 校验段名（fail-closed，防 task.Filename 穿越任务目录）。
	srcTnt := h.tenantFor(task.Owner)
	if srcTnt == nil {
		h.logger.Error("云任务源租户不可用（fail-closed）", "task_id", taskID, "owner", task.Owner)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to stat source file"}, http.StatusBadRequest)
		return
	}
	srcRel, ok := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
	if !ok {
		h.logger.Error("云任务源路径非法（fail-closed）", "task_id", taskID, "file", task.Filename)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to stat source file"}, http.StatusBadRequest)
		return
	}

	// 确定归档文件名（路径穿越防护 + 合法性检查）
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-task-%s-%d.tar.gz", taskID, time.Now().Unix())
	}
	// 路径穿越防护：仅允许文件名，拒绝 ../ 和 /
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive name"}, http.StatusBadRequest)
		return
	}
	// 确保以 .tar.gz 结尾
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}
	// 长度限制
	if len(archiveName) > 255 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive name too long"}, http.StatusBadRequest)
		return
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	owner := ActorFrom(r.Context())
	tnt := h.tenantFor(owner)
	if tnt == nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	root := tnt.Root()
	if err := root.MkdirAll("archive", 0755); err != nil {
		h.logger.Error("failed to create archive directory", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return
	}

	// 打包前：单文件总量限制（cloud_archive_max_bytes）。单文件仍受 addFileToTar 内
	// defaultMaxArchiveSize=100MB 约束；此处限制的是原始文件大小，与 addFileToTar 并存不冲突。
	// 审查 #10 结论（勿再分析）：此 os.Stat 是**必须的**大小校验，并非与 O_EXCL 重复的
	// 存在性预检——同名冲突由 openArchiveOutput 的 O_EXCL 单一负责（createTarGz 返回
	// errArchiveExists → 409）；此处 stat 失败即源文件不存在（400），职责不同，无冗余。
	info, err := srcTnt.Root().Stat(srcRel)
	if err != nil {
		h.logger.Error("failed to stat source file in cloud archive", "task_id", taskID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to stat source file"}, http.StatusBadRequest)
		return
	}
	if maxBytes := h.cloudArchiveMaxBytes(); maxBytes > 0 && info.Size() > maxBytes {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", info.Size(), maxBytes),
		}, http.StatusBadRequest)
		return
	}

	pre := info.Size() + cloudArchiveReservePlaceholder
	if reserveErr := h.storageMgr.TryReserve(pre, CategoryCloud); reserveErr != nil {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
		}, http.StatusInsufficientStorage)
		return
	}

	// 打包（流式 checksum）
	logger := h.logger.With("archive", "cloud_task", "task_id", taskID)
	checksum, err := createTarGz(srcTnt.Root(), srcRel, task.Filename, root, rel, logger)
	if err != nil {
		if errors.Is(err, errArchiveExists) {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏
			h.storageMgr.Release(pre, CategoryCloud)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		h.logger.Error("failed to create archive", "task_id", taskID, "error", err)
		_ = root.Remove(rel)
		h.storageMgr.Release(pre, CategoryCloud)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际大小对账预留配额：释放预占后按实际 size 重新预留，账本收敛到 actual。
	// 注意不能只释放 pre 而不补留——压缩后 actual 通常远小于 pre，若账本净为 0 会少计
	// actual 字节（配额窗口被放大，直到 30min 扫描校准）。
	h.storageMgr.Release(pre, CategoryCloud)
	if actual, statErr := root.Stat(rel); statErr == nil {
		if rErr := h.storageMgr.TryReserve(actual.Size(), CategoryCloud); rErr != nil {
			h.logger.Error("storage full, removing archive to keep ledger consistent", "task_id", taskID, "error", rErr)
			_ = root.Remove(rel)
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage for archive: %v", rErr),
			}, http.StatusInsufficientStorage)
			return
		}
	}

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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req CloudArchiveBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	if len(req.TaskIDs) == 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task_ids is required"}, http.StatusBadRequest)
		return
	}
	if len(req.TaskIDs) > 100 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "maximum 100 task IDs per request"}, http.StatusBadRequest)
		return
	}

	// 收集所有已完成任务的文件信息，跳过无效任务
	var files []fileWithRelPath
	var skippedTasks []string
	var totalSourceSize int64

	for _, taskID := range req.TaskIDs {
		// 使用 SnapshotTask 避免 data race；按请求者 owner 过滤（跨 owner 任务跳过，防内容外泄）
		task, ok := h.cloudMgr.SnapshotTask(taskID, ActorFrom(r.Context()))
		if !ok {
			h.logger.Warn("cloud batch archive: skipping task not found", "task_id", taskID)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		if task.Status != "completed" {
			h.logger.Warn("cloud batch archive: skipping task with non-completed status",
				"task_id", taskID, "status", task.Status)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）
		srcTnt := h.tenantFor(task.Owner)
		if srcTnt == nil {
			h.logger.Warn("cloud batch archive: skipping task with unavailable tenant",
				"task_id", taskID, "owner", task.Owner)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		srcRel, ok := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
		if !ok {
			h.logger.Warn("cloud batch archive: skipping task with invalid source path",
				"task_id", taskID, "file", task.Filename)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		// 顺带 stat 校验文件存在并累计原始大小（用于总量限制与配额预估）
		info, statErr := srcTnt.Root().Stat(srcRel)
		if statErr != nil {
			h.logger.Warn("cloud batch archive: skipping missing file",
				"task_id", taskID, "rel", srcRel, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		totalSourceSize += info.Size()

		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		files = append(files, fileWithRelPath{root: srcTnt.Root(), rel: srcRel, tarRel: relPath})
	}

	// 所有任务都被跳过则返回错误
	if len(files) == 0 {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "no valid tasks to archive",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return
	}

	// 确定归档文件名（路径穿越防护 + 合法性检查）
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-batch-%d.tar.gz", time.Now().Unix())
	}
	// 路径穿越防护：仅允许文件名，拒绝 ../ 和 /
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive name"}, http.StatusBadRequest)
		return
	}
	// 确保以 .tar.gz 结尾
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}
	// 长度限制
	if len(archiveName) > 255 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive name too long"}, http.StatusBadRequest)
		return
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	owner := ActorFrom(r.Context())
	tnt := h.tenantFor(owner)
	if tnt == nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	root := tnt.Root()
	if err := root.MkdirAll("archive", 0755); err != nil {
		h.logger.Error("failed to create archive directory", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return
	}

	// 打包前：总量限制（cloud_archive_max_bytes），受 max_storage_bytes 的 TryReserve 兜底
	if maxBytes := h.cloudArchiveMaxBytes(); maxBytes > 0 && totalSourceSize > maxBytes {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSourceSize, maxBytes),
		}, http.StatusBadRequest)
		return
	}

	pre := totalSourceSize + cloudArchiveReservePlaceholder
	if reserveErr := h.storageMgr.TryReserve(pre, CategoryCloud); reserveErr != nil {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
		}, http.StatusInsufficientStorage)
		return
	}

	// 多文件打包（流式 checksum）
	created := false
	logger := h.logger.With("archive", "cloud_batch")
	checksum, err := createMultiFileTarGz(files, root, rel, logger, &created)
	if err != nil {
		if !created {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏
			h.storageMgr.Release(pre, CategoryCloud)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		h.logger.Error("failed to create batch archive", "error", err)
		_ = root.Remove(rel)
		h.storageMgr.Release(pre, CategoryCloud)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际大小对账预留配额：释放预占后按实际 size 重新预留，账本收敛到 actual。
	// 注意不能只释放 pre 而不补留——压缩后 actual 通常远小于 pre，若账本净为 0 会少计
	// actual 字节（配额窗口被放大，直到 30min 扫描校准）。
	h.storageMgr.Release(pre, CategoryCloud)
	if actual, statErr := root.Stat(rel); statErr == nil {
		if rErr := h.storageMgr.TryReserve(actual.Size(), CategoryCloud); rErr != nil {
			h.logger.Error("storage full, removing archive to keep ledger consistent", "error", rErr)
			_ = root.Remove(rel)
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage for archive: %v", rErr),
			}, http.StatusInsufficientStorage)
			return
		}
	}

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

// fileWithRelPath 是源文件（租户根内相对路径）与 tar 内相对路径对。
// root 是源文件所在租户根（云任务文件按任务 owner 落租户 cloud/ 桶，跨 owner 任务 root 不同）。
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
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	if err := addFileToTar(tw, srcRoot, srcRel, srcTarName, logger); err != nil {
		return "", fmt.Errorf("add file to tar: %w", err)
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
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, f := range files {
		if err := addFileToTar(tw, f.root, f.rel, f.tarRel, logger); err != nil {
			return "", fmt.Errorf("add file %q to tar: %w", f.tarRel, err)
		}
	}

	succeeded = true
	checksum = hex.EncodeToString(hasher.Sum(nil))
	return checksum, nil
}

// errArchiveExists 归档输出文件已存在（O_EXCL 创建失败）。
var errArchiveExists = errors.New("archive file already exists")

// openArchiveOutput 以 O_EXCL 语义打开归档输出文件（相对租户 root 的 rel 路径）。
// 跨平台统一用 errors.Is(err, os.ErrExist) 判已存在（Windows 上 O_EXCL 对已存在文件同样报错）。
func openArchiveOutput(root *storage.Root, rel string) (*os.File, bool, error) {
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("create archive output: %w", err)
	}
	return f, true, nil
}
