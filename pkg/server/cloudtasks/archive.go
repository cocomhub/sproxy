// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// archive.go 是云归档（单任务 / 批量）的 HTTP 编排入口（P1-c S3a）：把已完成云任务
// 的产物打包为 tar.gz（落租户 archive 桶，或经统一转存写外部卷）。
//
// 分层纪律与 download.go/group.go 相同（docs/designs/2026-10-31-cloudtasks-deps-mapping.md
// §4/§9.5）：owner 由装配层解析后显式传入，响应写出用包内 writeJSON，行为与迁移前逐行
// 等价（状态码、错误文案、配额预留/结算顺序、archiveUsage 记账一律不变）。
//
// S3a 的临时接缝（S3b 随组归档/转存核心一并迁入后删除）：
//   - 配额预留/结算/释放与 archiveUsage 登记仍由装配层实现（Quota 接口方法委托）；
//   - tar 打包、转存执行核心（cloudArchiveToVolume）仍在装配层（Targets 接口方法委托），
//     其中 ArchiveToVolume 因核心尚未迁入只保留 HTTP 参数。
package cloudtasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/transfer"
)

// 归档扩展名与错误文案。文案与装配层 pkg/server/errors.go 逐字一致（既有测试断言
// message 与响应体字节）。
const (
	tarGZExt = ".tar.gz"

	msgArchiveStatFail = "failed to stat source file"
	msgArchiveDirFail  = "failed to create archive directory"
)

// CloudArchiveRequest 是 POST /api/cloud/tasks/{id}/archive 的请求体。
type CloudArchiveRequest struct {
	ArchiveName string `json:"archive_name,omitempty"`
	// TargetVolume 非空 = 归档打包后**转存到外部卷**（统一转存：归档=转存+可选预处理）。
	// 空 = 落租户 archive 桶（既有行为零回归）。
	TargetVolume string `json:"target_volume,omitempty"`
	// TargetPath 是转存目标在**目标卷 user 桶内**的相对路径（含文件名；可含子目录）。
	// 空 = 默认 `archive/<archive_name>`；以 "/" 结尾 = 目录（拼上 archive_name）。
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

// ArchiveExtras 是「仅转存外部卷支持」的选项集合（校验入参收拢为结构体，避免长参数列表）。
// 装配层存档同名结构（archiveExtras）由 Targets.ValidateArchiveExtras 适配，校验规则只有一份。
type ArchiveExtras struct {
	TargetVolume string
	TargetPath   string
	Encrypt      bool
	Cipher       string
	Compression  string
}

// ArchiveSource 是一个待打包的源文件：租户根 + 根内相对路径 + tar 内条目名。
//
// 装配层把本类型适配为内部的 fileWithRelPath（tar 助手仍在装配层），字段一一对应。
type ArchiveSource struct {
	Root   *storage.Root
	Rel    string // 根内相对源路径（cloud/<taskID>/<file>）
	TarRel string // tar 内条目名
}

// ArchiveVolumeTransfer 是统一转存的入参（S107 收敛：字段替代长参数列表）。
// 装配层的 archiveVolumeTransfer 是它的类型别名（唯一实现，S3b 迁入后别名删除）。
type ArchiveVolumeTransfer struct {
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

// ArchiveTask 处理 POST /api/cloud/tasks/{id}/archive。
// 将已完成云下载任务的文件打包为 tar.gz 归档文件。
//
// 排除面边界（F5 review 成文）：cloud 源（cloud 桶）与 archive 输出（archive 桶）均落**默认卷**
// （Tenants.TenantFor），属 §11「cloud/archive 产物落默认卷」设计内例外——默认卷被 ACL 排除的
// owner 仍可经自建归档回读默认卷上自己生成的 cloud 内容。见 volumes.go defaultVolumeAllows 边界注释。
// 不改行为，仅记录避免未来误判为漏洞。
func (s *Service) ArchiveTask(w http.ResponseWriter, r *http.Request, owner string) {
	taskID := r.PathValue("id")

	// 校验任务存在且状态为 completed、解析请求体、推导任务源路径（失败已回响应）。
	task, req, srcTnt, srcRel, ok := s.archivePrepare(w, r, owner, taskID)
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
	// 审查 #10 结论（勿再分析）：此 Stat 是**必须的**大小校验，并非与 O_EXCL 重复的
	// 存在性预检——同名冲突由 openArchiveOutput 的 O_EXCL 单一负责（createTarGz 返回
	// errArchiveExists → 409）；此处 stat 失败即源文件不存在（400），职责不同，无冗余。
	info, err := srcTnt.Root().Stat(srcRel)
	if err != nil {
		s.deps.Logger.Error("failed to stat source file in cloud archive", "task_id", taskID, "error", err)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return
	}

	// 统一转存：target_volume → 打包转存外部卷（统一「归档 = 转存 + 预处理」管道），
	// 不落租户 archive 桶。target_path/encrypt/cipher/compression 仅与 target_volume 同用。
	if !s.validateArchiveExtras(w, archiveExtrasOf(req.TargetVolume, req.TargetPath, req.Encrypt, req.Cipher, req.Compression)) {
		return
	}
	if req.TargetVolume != "" {
		s.archiveTaskToVolume(w, r, owner, taskID, task, req, srcTnt, srcRel, info, archiveName)
		return
	}

	// 确保输出目录存在 + 推导输出 rel（失败已回响应）。
	root, rel, archiveName, ok := s.archiveOutput(w, owner, archiveName)
	if !ok {
		return
	}

	// 总量限制 + 预留配额（scope 优先，storageMgr 回退；与批量/组归档同款逻辑）。
	res, pre, ok := s.reserveArchiveQuota(r.Context(), w, owner, info.Size())
	if !ok {
		return
	}

	// 打包（流式 checksum）
	logger := s.deps.Logger.With("archive", "cloud_task", "task_id", taskID)
	checksum, exists, err := s.deps.Targets.CreateArchiveTar(srcTnt.Root(), srcRel, task.Filename, root, rel, logger)
	if err != nil || exists {
		if exists {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			s.deps.Quota.ReleaseArchive(res, pre)
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		s.deps.Logger.Error("failed to create archive", "task_id", taskID, "error", err)
		_ = root.Remove(rel)
		s.deps.Quota.ReleaseArchive(res, pre)
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际对账预留配额（scope 优先 Commit(actual)，storageMgr 回退 3 步；失败已回 507）。
	actual, ok := s.settleArchiveQuota(w, root, rel, res, pre, s.deps.Logger.With("task_id", taskID))
	if !ok {
		return
	}

	// P5 归档占用登记（登记后释放 Scope 不再依赖周期扫描自愈）。
	s.deps.Quota.RecordArchiveUsage(owner, archiveName, actual)

	archiveInfo, err := root.Stat(rel)
	if err != nil {
		s.deps.Logger.Error("failed to stat archive", "task_id", taskID, "error", err)
	}

	archiveSize := int64(0)
	if archiveInfo != nil {
		archiveSize = archiveInfo.Size()
	}

	s.writeJSON(w, CloudArchiveResult{
		Success:   true,
		File:      archiveName,
		Size:      archiveSize,
		Checksum:  checksum,
		TaskCount: 1,
	}, http.StatusOK)
}

// ArchiveBatch 处理 POST /api/cloud/archive。
// 批量将多个已完成云下载任务的文件打包为单个 tar.gz 归档文件。
func (s *Service) ArchiveBatch(w http.ResponseWriter, r *http.Request, owner string) {
	// 解析请求体 + 校验 task_ids 数量（失败已回响应）。
	req, ok := s.archiveBatchParse(w, r)
	if !ok {
		return
	}

	// 收集所有已完成任务的文件信息，跳过无效任务（全部跳过已回响应）。
	sources, skippedTasks, totalSourceSize, ok := s.archiveCollectTasks(w, owner, req.TaskIDs)
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
	if !s.validateArchiveExtras(w, archiveExtrasOf(req.TargetVolume, req.TargetPath, req.Encrypt, req.Cipher, req.Compression)) {
		return
	}
	if req.TargetVolume != "" {
		s.archiveBatchToVolume(w, r, owner, req, sources, totalSourceSize, archiveName)
		return
	}

	// 确保输出目录存在 + 推导输出 rel（失败已回响应）。
	root, rel, archiveName, ok := s.archiveOutput(w, owner, archiveName)
	if !ok {
		return
	}

	// 打包前：总量限制 + 预留配额（scope 优先，storageMgr 回退；与单任务/组归档一致）。
	res, pre, ok := s.reserveArchiveQuota(r.Context(), w, owner, totalSourceSize)
	if !ok {
		return
	}

	// 多文件打包（流式 checksum）
	logger := s.deps.Logger.With("archive", "cloud_batch")
	checksum, exists, err := s.deps.Targets.CreateMultiArchiveTar(sources, root, rel, logger)
	if err != nil || exists {
		if exists {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			s.deps.Quota.ReleaseArchive(res, pre)
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		s.deps.Logger.Error("failed to create batch archive", "error", err)
		_ = root.Remove(rel)
		s.deps.Quota.ReleaseArchive(res, pre)
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际对账预留配额（TryReserve 拒绝已回 507）。
	actual, ok := s.settleArchiveQuota(w, root, rel, res, pre, s.deps.Logger)
	if !ok {
		return
	}

	// P5 归档占用登记（删除组时按登记 Release Scope）。
	s.deps.Quota.RecordArchiveUsage(owner, archiveName, actual)

	info, err := root.Stat(rel)
	if err != nil {
		s.deps.Logger.Error("failed to stat archive", "error", err)
	}

	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	s.writeJSON(w, CloudArchiveResult{
		Success:      true,
		File:         archiveName,
		Size:         size,
		Checksum:     checksum,
		TaskCount:    len(sources),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// archivePrepare 校验云任务（存在 + completed + 归属请求者）、解析请求体、
// 推导任务源文件路径（任务 owner 租户 cloud 桶）。任一失败已写响应并返回 ok=false。
func (s *Service) archivePrepare(w http.ResponseWriter, r *http.Request, owner, taskID string) (task *cloud.CloudTask, req CloudArchiveRequest, srcTnt *storage.Tenant, srcRel string, ok bool) {
	// 校验任务存在且状态为 completed（使用 SnapshotTask 避免 data race）。
	// 按请求者 owner 过滤：跨 owner 任务视为不存在（404 防枚举）。
	var found bool
	task, found = s.deps.Tasks.SnapshotTask(taskID, owner)
	if !found {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "task not found"}, http.StatusNotFound)
		return task, req, nil, "", false
	}
	if task.Status != "completed" {
		s.writeJSON(w, CloudArchiveResult{
			Success: false,
			Message: fmt.Sprintf("task status is %q, expected \"completed\"", task.Status),
		}, http.StatusBadRequest)
		return task, req, nil, "", false
	}

	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return task, req, nil, "", false
	}

	// 构造源文件路径（任务文件按任务 owner 落租户 cloud/ 桶）：根内 rel = cloud/<taskID>/<file>
	// 经 FeatureRel 校验段名（fail-closed，防 task.Filename 穿越任务目录）。
	srcTnt = s.deps.Tenants.TenantFor(task.Owner)
	if srcTnt == nil {
		s.deps.Logger.Error("云任务源租户不可用（fail-closed）", "task_id", taskID, "owner", task.Owner)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	var srcOK bool
	srcRel, srcOK = srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
	if !srcOK {
		s.deps.Logger.Error("云任务源路径非法（fail-closed）", "task_id", taskID, "file", task.Filename)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveStatFail}, http.StatusBadRequest)
		return task, req, nil, "", false
	}
	return task, req, srcTnt, srcRel, true
}

// archiveOutput 解析归档输出路径：防穿越校验归档名（加 .tar.gz 后缀、限长）、
// 确保租户 archive 桶目录存在并推导输出 rel。失败时已写响应并返回 ok=false。
func (s *Service) archiveOutput(w http.ResponseWriter, owner, archiveName string) (root *storage.Root, rel, name string, ok bool) {
	// 路径穿越防护 + 后缀/长度校验（与统一转存共用同一校验）。
	name, msg := NormalizeArchiveName(archiveName)
	if msg != "" {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
		return nil, "", "", false
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<owner>/archive/）
	tnt := s.deps.Tenants.TenantFor(owner)
	if tnt == nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", "", false
	}
	root = tnt.Root()
	if err := root.MkdirAll("archive", 0755); err != nil {
		s.deps.Logger.Error(msgArchiveDirFail, "error", err)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", "", false
	}
	var relOK bool
	rel, relOK = tnt.FeatureRel("archive", name)
	if !relOK {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return nil, "", "", false
	}
	return root, rel, name, true
}

// NormalizeArchiveName 校验并归一归档名：仅文件名（Base，防 ../ 与 /）、补 .tar.gz
// 后缀、长度 ≤255；返回 (归一后的名字, 错误文案)，文案为空即通过。bucket 落盘与统一转存
// 共用，防两套校验漂移。
//
// 导出理由：组归档路径（S3b 前仍在装配层）经此取用同一实现；随 S3b 迁入后转为包内函数。
func NormalizeArchiveName(archiveName string) (string, string) {
	// 路径穿越防护：仅允许文件名，拒绝 ../ 和 /
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		return "", "invalid archive name"
	}
	// 确保以 .tar.gz 结尾
	if !strings.HasSuffix(archiveName, tarGZExt) {
		archiveName += tarGZExt
	}
	// 长度限制
	if len(archiveName) > 255 {
		return "", "archive name too long"
	}
	return archiveName, ""
}

// archiveTaskToVolume 单任务归档统一转存：单文件（tar.gz 打包）→ 外部卷。
// ACL 与组/批量归档同语义（Volumes.Allowed 防跨租户写私有卷）。
func (s *Service) archiveTaskToVolume(w http.ResponseWriter, r *http.Request, owner, taskID string,
	task *cloud.CloudTask, req CloudArchiveRequest, srcTnt *storage.Tenant, srcRel string,
	info os.FileInfo, archiveName string,
) {
	if !s.deps.Volumes.Allowed(owner, req.TargetVolume) {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
		return
	}
	name, msg := NormalizeArchiveName(archiveName)
	if msg != "" {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
		return
	}
	// R25：用户可见读入口（加密卷透明解密）——不得直调 root.Open。
	f, oerr := rootfs.OpenUserVisibleRead(srcTnt.Root(), srcRel)
	if oerr != nil {
		s.deps.Logger.Error("统一转存打开源文件失败", "task_id", taskID, "error", oerr)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "open source failed: " + oerr.Error()}, http.StatusInternalServerError)
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
	s.deps.Targets.ArchiveToVolume(w, r, ArchiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: name,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: size,
	}, s.deps.Logger.With("archive", "cloud_task", "task_id", taskID), nil)
}

// archiveBatchToVolume 批量归档统一转存：多文件（tar.gz 打包）→ 外部卷。
func (s *Service) archiveBatchToVolume(w http.ResponseWriter, r *http.Request, owner string,
	req CloudArchiveBatchRequest, sources []ArchiveSource, totalSize int64, archiveName string,
) {
	if !s.deps.Volumes.Allowed(owner, req.TargetVolume) {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
		return
	}
	name, msg := NormalizeArchiveName(archiveName)
	if msg != "" {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
		return
	}
	entries, err := s.deps.Targets.ArchiveEntries(sources)
	if err != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "open source failed: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	s.deps.Targets.ArchiveToVolume(w, r, ArchiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: name,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: totalSize,
	}, s.deps.Logger.With("archive", "cloud_batch"), nil)
}

// archiveBatchParse 解析批量归档请求体并校验 task_ids 数量（1..100）。
// 失败已写响应并返回 ok=false。
func (s *Service) archiveBatchParse(w http.ResponseWriter, r *http.Request) (req CloudArchiveBatchRequest, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return req, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainBody(r); err != nil {
		s.writeJSON(w, uploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return req, false
	}

	if len(req.TaskIDs) == 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "task_ids is required"}, http.StatusBadRequest)
		return req, false
	}
	if len(req.TaskIDs) > 100 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "maximum 100 task IDs per request"}, http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// archiveCollectTasks 逐任务收集已完成任务的文件信息（无效任务跳过并记 skipped）。
// 所有任务都被跳过时已写响应并返回 ok=false。
func (s *Service) archiveCollectTasks(w http.ResponseWriter, owner string, taskIDs []string) (sources []ArchiveSource, skippedTasks []string, totalSourceSize int64, ok bool) {
	for _, taskID := range taskIDs {
		src, size, skip := s.archiveTaskSource(owner, taskID)
		if skip {
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		totalSourceSize += size
		sources = append(sources, src)
	}

	// 所有任务都被跳过则返回错误
	if len(sources) == 0 {
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: "no valid tasks to archive",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return nil, skippedTasks, totalSourceSize, false
	}
	return sources, skippedTasks, totalSourceSize, true
}

// archiveTaskSource 解析批量归档单个任务的文件信息（快照 + completed + 租户/路径/
// 存在性校验）。解析失败记 Warn 日志并返回 skip=true（调用方累积 skipped 列表）。
func (s *Service) archiveTaskSource(owner, taskID string) (ArchiveSource, int64, bool) {
	// 使用 SnapshotTask 避免 data race；按请求者 owner 过滤（跨 owner 任务跳过，防内容外泄）。
	task, found := s.deps.Tasks.SnapshotTask(taskID, owner)
	if !found {
		s.deps.Logger.Warn("cloud batch archive: skipping task not found", "task_id", taskID)
		return ArchiveSource{}, 0, true
	}
	if task.Status != "completed" {
		s.deps.Logger.Warn("cloud batch archive: skipping task with non-completed status",
			"task_id", taskID, "status", task.Status)
		return ArchiveSource{}, 0, true
	}

	// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）
	srcTnt := s.deps.Tenants.TenantFor(task.Owner)
	if srcTnt == nil {
		s.deps.Logger.Warn("cloud batch archive: skipping task with unavailable tenant",
			"task_id", taskID, "owner", task.Owner)
		return ArchiveSource{}, 0, true
	}
	srcRel, relOK := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
	if !relOK {
		s.deps.Logger.Warn("cloud batch archive: skipping task with invalid source path",
			"task_id", taskID, "file", task.Filename)
		return ArchiveSource{}, 0, true
	}

	// 顺带 stat 校验文件存在并累计原始大小（用于总量限制与配额预估）
	info, statErr := srcTnt.Root().Stat(srcRel)
	if statErr != nil {
		s.deps.Logger.Warn("cloud batch archive: skipping missing file",
			"task_id", taskID, "rel", srcRel, "error", statErr)
		return ArchiveSource{}, 0, true
	}

	relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
	return ArchiveSource{Root: srcTnt.Root(), Rel: srcRel, TarRel: relPath}, info.Size(), false
}

// validateArchiveExtras 校验「仅转存外部卷支持」的选项；失败已写 400 响应。
func (s *Service) validateArchiveExtras(w http.ResponseWriter, extras ArchiveExtras) bool {
	status, msg := s.deps.Targets.ValidateArchiveExtras(extras)
	if status != 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return false
	}
	return true
}

// archiveExtrasOf 把归档请求的转存选项收拢为校验入参。
func archiveExtrasOf(targetVolume, targetPath string, encrypt bool, cipher, compression string) ArchiveExtras {
	return ArchiveExtras{
		TargetVolume: targetVolume, TargetPath: targetPath,
		Encrypt: encrypt, Cipher: cipher, Compression: compression,
	}
}

// reserveArchiveQuota 预留归档输出所需配额（总量上限 + scope 优先 / storageMgr 回退）。
// 失败已写响应并返回 ok=false（文案与状态码由 Quota.ReserveArchive 给出，逐字等价迁移前）。
func (s *Service) reserveArchiveQuota(ctx context.Context, w http.ResponseWriter, owner string, totalSourceSize int64) (*quota.Settlement, int64, bool) {
	res, pre, status, msg := s.deps.Quota.ReserveArchive(ctx, owner, totalSourceSize)
	if status != 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return nil, 0, false
	}
	return res, pre, true
}

// settleArchiveQuota 按磁盘实际大小对账预留配额；失败已写 507 响应并返回 ok=false。
func (s *Service) settleArchiveQuota(w http.ResponseWriter, root *storage.Root, rel string,
	res *quota.Settlement, pre int64, logger *slog.Logger,
) (int64, bool) {
	actual, status, msg := s.deps.Quota.SettleArchive(root, rel, res, pre, logger)
	if status != 0 {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, status)
		return 0, false
	}
	return actual, true
}
