// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// cloudArchiveTask 处理 POST /api/cloud/tasks/{id}/archive。
// 将已完成云下载任务的文件打包为 tar.gz 归档文件。
func (h *Handlers) cloudArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	// 校验任务存在且状态为 completed（使用 SnapshotTask 避免 data race）
	task, ok := h.cloudMgr.SnapshotTask(taskID)
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

	// 构造源文件路径
	cloudDir := filepath.Join(h.cloudMgr.uploadsDir, cloudDirName)
	sourceFile := filepath.Join(cloudDir, task.ID, task.Filename)
	sourceDir := filepath.Join(cloudDir, task.ID)

	// 路径穿越防护
	if !IsPathWithin(sourceFile, sourceDir) {
		h.logger.Error("path traversal detected in cloud archive",
			"task_id", taskID, "source_file", sourceFile)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid file path"}, http.StatusInternalServerError)
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

	// 确保输出目录存在
	archiveDir := filepath.Join(h.cloudMgr.uploadsDir, cloudArchiveDirName)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		h.logger.Error("failed to create archive directory", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	outputPath := filepath.Join(archiveDir, archiveName)
	// 二次验证：确保 outputPath 仍在 archiveDir 内
	if !strings.HasPrefix(filepath.Clean(outputPath), filepath.Clean(archiveDir)+string(filepath.Separator)) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return
	}

	// 打包
	logger := h.logger.With("archive", "cloud_task", "task_id", taskID)
	if err := createTarGz(sourceFile, task.Filename, outputPath, logger); err != nil {
		h.logger.Error("failed to create archive", "task_id", taskID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 计算归档文件 checksum 和大小
	checksum, err := FileChecksum(outputPath)
	if err != nil {
		h.logger.Error("failed to compute archive checksum", "task_id", taskID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive created but checksum failed"}, http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		h.logger.Error("failed to stat archive", "task_id", taskID, "error", err)
	}

	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:   true,
		File:      filepath.ToSlash(filepath.Join(cloudArchiveDirName, archiveName)),
		Size:      size,
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

	if len(req.TaskIDs) == 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task_ids is required"}, http.StatusBadRequest)
		return
	}
	if len(req.TaskIDs) > 100 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "maximum 100 task IDs per request"}, http.StatusBadRequest)
		return
	}

	// 收集所有已完成任务的文件信息，跳过无效任务
	cloudDir := filepath.Join(h.cloudMgr.uploadsDir, cloudDirName)
	var files []fileWithRelPath
	var skippedTasks []string

	for _, taskID := range req.TaskIDs {
		// 使用 SnapshotTask 避免 data race
		task, ok := h.cloudMgr.SnapshotTask(taskID)
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

		sourceFile := filepath.Join(cloudDir, task.ID, task.Filename)
		sourceDir := filepath.Join(cloudDir, task.ID)

		// 路径穿越防护
		if !IsPathWithin(sourceFile, sourceDir) {
			h.logger.Error("path traversal detected in cloud batch archive",
				"task_id", taskID, "source_file", sourceFile)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		files = append(files, fileWithRelPath{fullPath: sourceFile, relPath: relPath})
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

	// 确保输出目录存在
	archiveDir := filepath.Join(h.cloudMgr.uploadsDir, cloudArchiveDirName)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		h.logger.Error("failed to create archive directory", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	outputPath := filepath.Join(archiveDir, archiveName)
	// 二次验证：确保 outputPath 仍在 archiveDir 内
	if !strings.HasPrefix(filepath.Clean(outputPath), filepath.Clean(archiveDir)+string(filepath.Separator)) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return
	}

	// 多文件打包
	logger := h.logger.With("archive", "cloud_batch")
	if err := createMultiFileTarGz(files, outputPath, logger); err != nil {
		h.logger.Error("failed to create batch archive", "error", err)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 计算 checksum 和大小
	checksum, err := FileChecksum(outputPath)
	if err != nil {
		h.logger.Error("failed to compute archive checksum", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive created but checksum failed"}, http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		h.logger.Error("failed to stat archive", "error", err)
	}

	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:      true,
		File:         filepath.ToSlash(filepath.Join(cloudArchiveDirName, archiveName)),
		Size:         size,
		Checksum:     checksum,
		TaskCount:    len(files),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// fileWithRelPath 是文件路径与 tar 内相对路径对。
type fileWithRelPath struct {
	fullPath string
	relPath  string
}

// createTarGz 将单个文件打包为 tar.gz。
// 使用 succeeded 标记模式确保出错时清理输出文件。
func createTarGz(sourceFile, sourceName, outputPath string, logger *slog.Logger) (err error) {
	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			os.Remove(outputPath)
		}
	}()

	gw := gzip.NewWriter(outputFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	if err := addFileToTar(tw, sourceFile, sourceName, logger); err != nil {
		return fmt.Errorf("add file to tar: %w", err)
	}

	succeeded = true
	return nil
}

// createMultiFileTarGz 将多个文件打包为单个 tar.gz。
// 使用 succeeded 标记模式确保出错时清理输出文件。
func createMultiFileTarGz(files []fileWithRelPath, outputPath string, logger *slog.Logger) (err error) {
	outputFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer outputFile.Close()

	// 出错时清理输出文件
	succeeded := false
	defer func() {
		if !succeeded {
			os.Remove(outputPath)
		}
	}()

	gw := gzip.NewWriter(outputFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, f := range files {
		if err := addFileToTar(tw, f.fullPath, f.relPath, logger); err != nil {
			return fmt.Errorf("add file %q to tar: %w", f.relPath, err)
		}
	}

	succeeded = true
	return nil
}
