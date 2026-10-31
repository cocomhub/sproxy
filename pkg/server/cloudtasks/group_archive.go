// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// group_archive.go 是云**组归档**的 HTTP 编排入口（P1-c S3b）：把组内已完成子任务的
// 产物打包为单个 tar.gz（落租户 archive 桶，或经统一转存写目标卷）。
//
// 分层纪律与 archive.go 相同（docs/designs/2026-10-31-cloudtasks-deps-mapping.md
// §4/§9.5）：owner 由装配层解析后显式传入，响应写出用包内 writeJSON，行为与迁移前逐行
// 等价（状态码、错误文案、配额预留/结算顺序、archiveUsage 记账、组归档名落库一律不变）。
//
// S3c 后 tar 打包助手仍在装配层（Targets.CreateMultiArchiveTar），配额预留/对账/释放经
// Quota 接口取用；转存执行核心、entries/size 助手与 archiveUsage 登记已在本包（transfer.go）。
package cloudtasks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// ArchiveGroup 处理 POST /api/cloud/groups/{id}/archive。
// 收集组内所有已完成子任务的文件打包为单个 tar.gz（未完成任务跳过并记录）。
func (s *Service) ArchiveGroup(w http.ResponseWriter, r *http.Request, owner string) {
	groupID := r.PathValue("id")
	if _, ok := s.deps.Groups.GetGroup(groupID, owner); !ok {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 解析请求体
	req, ok := s.archiveGroupParseRequest(w, r)
	if !ok {
		return
	}

	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName, ok := s.archiveGroupResolveName(w, req, groupID)
	if !ok {
		return
	}

	// 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户 cloud/ 桶）
	sources, skippedTasks, totalSourceSize, ok := s.archiveGroupCollectFiles(w, groupID, owner)
	if !ok {
		return
	}

	if len(sources) == 0 {
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: "no completed files to archive in group",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return
	}

	// 统一转存：target_volume 非空 → 打包后转存到目标卷（复用 pkg/transfer 抽象），
	// 不落租户 archive 桶。否则落桶（既有行为零回归）。
	// V4：转存目标卷须经 ACL 校验（Volumes.Allowed 防跨租户写私有卷）——与单条/批量
	// 入口同语义。target_path/encrypt/cipher/compression 仅与 target_volume 同用（防静默忽略）。
	if !s.validateArchiveExtras(w, archiveExtrasOf(req.TargetVolume, req.TargetPath, req.Encrypt, req.Cipher, req.Compression)) {
		return
	}
	if req.TargetVolume != "" {
		if !s.deps.Volumes.Allowed(owner, req.TargetVolume) {
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "目标卷对当前用户不可用（ACL 拒绝）"}, http.StatusForbidden)
			return
		}
		s.ArchiveGroupToVolume(w, r, groupID, owner, sources, req, archiveName)
		return
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	root, rel, ok := s.archiveGroupPrepTarget(w, owner, req, archiveName)
	if !ok {
		return
	}

	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	res, pre, ok := s.reserveArchiveQuota(r.Context(), w, owner, totalSourceSize)
	if !ok {
		return
	}

	// 多文件打包
	checksum, ok := s.archiveGroupCreateTar(w, sources, root, rel, res, pre, groupID)
	if !ok {
		return
	}

	// 按磁盘实际大小对账预留配额（与单任务/批量归档同一实现：Reconcile(actual) + Commit()，
	// 多预留部分自动归还；storageMgr 回退保留 3 步 Release(pre)+TryReserve(actual)）。
	actual, ok := s.settleArchiveQuota(w, root, rel, res, pre, s.deps.Logger.With("group_id", groupID))
	if !ok {
		return
	}

	// P5 归档占用登记（删除时按登记释放 Scope，不再依赖周期扫描自愈）。
	s.recordArchiveUsage(owner, archiveName, actual)

	// 更新组归档路径（落库到真实组对象；仅存归档名，客户端不接触 .__ 内部路径）
	s.deps.Groups.SetGroupArchiveFile(groupID, archiveName)

	s.writeJSON(w, CloudArchiveResult{
		Success:      true,
		File:         archiveName,
		Size:         actual,
		Checksum:     checksum,
		TaskCount:    len(sources),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// archiveGroupParseRequest 解析归档请求体（限 1 MiB + bodyValidator EOF 哈希校验）。
// 返回 false 表示已回错误响应包。
func (s *Service) archiveGroupParseRequest(w http.ResponseWriter, r *http.Request) (req CloudArchiveRequest, ok bool) {
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
	return req, true
}

// archiveGroupResolveName 确定归档文件名（默认使用 groupID 保证唯一性；路径穿越防护 +
// 长度校验 + 补 .tar.gz 后缀）。返回 false 表示已回错误响应包。
func (s *Service) archiveGroupResolveName(w http.ResponseWriter, req CloudArchiveRequest, groupID string) (string, bool) {
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("%s-%d.tar.gz", groupID, time.Now().Unix())
	}
	name, msg := NormalizeArchiveName(archiveName)
	if msg != "" {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msg}, http.StatusBadRequest)
		return "", false
	}
	return name, true
}

// archiveGroupCollectFiles 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户
// cloud/ 桶；未完成任务跳过并记录；返回 false 表示已回错误响应包）。
func (s *Service) archiveGroupCollectFiles(w http.ResponseWriter, groupID, owner string) (sources []ArchiveSource, skippedTasks []string, totalSourceSize int64, ok bool) {
	group, ok := s.deps.Groups.GetGroup(groupID, owner)
	if !ok {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return nil, nil, 0, false
	}
	for _, taskID := range group.TaskIDs {
		task, found := s.deps.Tasks.SnapshotTask(taskID, owner)
		if !found {
			s.deps.Logger.Warn("cloud group archive: skipping task not found", "group_id", groupID, "task_id", taskID)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		if task.Status != "completed" {
			s.deps.Logger.Warn("cloud group archive: skipping task with non-completed status",
				"group_id", groupID, "task_id", taskID, "status", task.Status)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）。
		// FeatureRel 逐段校验（fail-closed，防 task.Filename 穿越任务目录）。
		srcTnt := s.deps.Tenants.TenantFor(task.Owner)
		if srcTnt == nil {
			s.deps.Logger.Warn("cloud group archive: skipping task with unavailable tenant",
				"group_id", groupID, "task_id", taskID, "owner", task.Owner)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		srcRel, relOK := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
		if !relOK {
			s.deps.Logger.Warn("cloud group archive: skipping task with invalid source path",
				"group_id", groupID, "task_id", taskID, "file", task.Filename)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		// 收集后、打包前文件可能被删除/替换，先确认存在并统计大小（用于总量限制与配额预估）
		if info, statErr := srcTnt.Root().Stat(srcRel); statErr != nil {
			s.deps.Logger.Warn("cloud group archive: skipping missing file",
				"group_id", groupID, "task_id", taskID, "rel", srcRel, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
		} else {
			totalSourceSize += info.Size()
		}
		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		sources = append(sources, ArchiveSource{Root: srcTnt.Root(), Rel: srcRel, TarRel: relPath})
	}
	return sources, skippedTasks, totalSourceSize, true
}

// archiveGroupPrepTarget 确保输出目录存在并解析归档目标相对路径（租户 archive 桶），
// 含用户指定名时的同名冲突检查。返回 false 表示已回错误响应包。
func (s *Service) archiveGroupPrepTarget(w http.ResponseWriter, owner string, req CloudArchiveRequest, archiveName string) (*storage.Root, string, bool) {
	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	tnt := s.deps.Tenants.TenantFor(owner)
	if tnt == nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	root := tnt.Root()
	if mkErr := root.MkdirAll("archive", 0755); mkErr != nil {
		s.deps.Logger.Error(msgArchiveDirFail, "error", mkErr)
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	rel, relOK := tnt.FeatureRel("archive", archiveName)
	if !relOK {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return nil, "", false
	}
	// 用户指定归档名时，校验同名文件是否已存在，存在则拒绝（落在租户 archive 桶下，
	// 审查 F3 语义保留：不查全局根，避免误 409）
	if req.ArchiveName != "" {
		if _, err := root.Stat(rel); err == nil {
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "archive file already exists: " + archiveName}, http.StatusConflict)
			return nil, "", false
		}
	}
	return root, rel, true
}

// archiveGroupCreateTar 执行多文件打包（流式 checksum；O_EXCL 同名冲突与失败路径释放
// 预留并清理半成品）。返回 false 表示已回错误响应包。
func (s *Service) archiveGroupCreateTar(w http.ResponseWriter, sources []ArchiveSource, root *storage.Root, rel string,
	res *quota.Settlement, pre int64, groupID string,
) (string, bool) {
	logger := s.deps.Logger.With("archive", "group", "group_id", groupID)
	checksum, exists, err := s.deps.Targets.CreateMultiArchiveTar(sources, root, rel, logger)
	if err != nil || exists {
		if exists {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			s.deps.Quota.ReleaseArchive(res, pre)
			s.writeJSON(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return "", false
		}
		s.deps.Logger.Error("failed to create group archive", "group_id", groupID, "error", err)
		_ = root.Remove(rel)
		s.deps.Quota.ReleaseArchive(res, pre)
		s.writeJSON(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return "", false
	}
	return checksum, true
}

// ArchiveGroupToVolume 统一转存：组归档打包后转存到目标卷（target_volume）。
// 收集组文件 Reader 后交共享核心（archiveToVolume）。
//
// 导出理由：装配层既有直调测试（cloud_download_handler_test.go 的组归档转存用例）经此
// 驱动同一实现（含目标卷/可信卷接线断言），与 TransferACLRejection 的导出理由同构。
func (s *Service) ArchiveGroupToVolume(w http.ResponseWriter, r *http.Request, groupID, owner string,
	sources []ArchiveSource, req CloudArchiveRequest, archiveName string,
) {
	entries, err := archiveEntries(sources)
	if err != nil {
		s.writeJSON(w, CloudArchiveResult{Success: false, Message: "open source failed: " + err.Error()}, http.StatusInternalServerError)
		return
	}
	// 成功时登记组归档引用（供组删除联动清理转存产物；转存 URL 含卷信息）。
	onSuccess := func(url string) {
		s.deps.Groups.SetGroupArchiveFile(groupID, url)
	}
	s.archiveToVolume(w, r, ArchiveVolumeTransfer{
		Owner: owner, TargetVolume: req.TargetVolume, TargetPath: req.TargetPath, ArchiveName: archiveName,
		Encrypt: req.Encrypt, Cipher: req.Cipher, Compression: req.Compression,
		Entries: entries, TotalSize: archiveTotalSize(sources),
	}, s.deps.Logger.With("archive", "group", "group_id", groupID), onSuccess)
}
