// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// delete.go 是删除族的 **HTTP 面**：`POST /delete`（薄适配）与 `POST /api/batch/delete`
// 的 DTO 与逐条处理，自 `pkg/server/delete_handler.go` 整族迁入（接收者由 *Handlers 改为
// *Service）。
//
// 分工（D-2 第 4 片）：**领域逻辑在 write_ops.go 的 `DeleteFile`**（文件级互斥、跨卷定位、
// checksum 门禁、配额/卷池/台账释放、计量与审计），本文件只保留「解析请求 → 调域方法 →
// 写响应」与批量族的逐条结果聚合。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// errMsgEmptyFilename 是 filename 查询参数缺失时的响应文案（随本族自 pkg/server/errors.go
// 迁入）：与 pkg/server.errors.go 同值——该常量在装配层另有 2 个消费者（下载路径解析），
// 故两侧各留一份（等价副本，只影响文案字面量，与其余 errMsg* 副本同一约定）。
const errMsgEmptyFilename = "文件名不能为空"

// BatchDeleteRequest 批量删除请求体。
type BatchDeleteRequest struct {
	Files []BatchDeleteFile `json:"files"`
}

// BatchDeleteFile 批量删除中的单条文件。
type BatchDeleteFile struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
}

// Delete 处理 POST /delete?filename=<name>[&volume=<v>]。
// 要求 X-File-Checksum 头与文件实际 checksum 匹配才删除（防误删）。
//
// 本处理器只做「解析入参 → 调域方法 → 写响应」；全部领域逻辑（文件级互斥、跨卷定位、
// checksum 门禁、配额/卷池/台账释放、审计）在 write_ops.go 的 DeleteFile 内。
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	res, err := s.DeleteFile(r.Context(), DeleteFileInput{
		Owner:            s.rt.actorOf(r),
		RemotePath:       r.URL.Query().Get("filename"),
		ExpectedChecksum: r.Header.Get(headerFileChecksum),
		ExplicitVol:      r.URL.Query().Get("volume"),
	})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: res.Message}, http.StatusOK)
}

// processBatchDeleteItem 处理单条批量删除：**在域方法之上**循环（P2-c）。
//
// 语义与错误聚合逐字不变：路径校验、跨卷定位、checksum 门禁、配额/卷池/checksum 台账释放
// 由 DeleteFile 承担；批量族的两项**历史差异**以入参显式表达——`AllowMissing`（缺文件按幂等
// 成功）与 `SkipFileLock`（不因并发上传把整批变成 409），本函数只做文案映射与结果聚合。
func (s *Service) processBatchDeleteItem(ctx context.Context, owner string, f BatchDeleteFile) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	res, err := s.DeleteFile(ctx, DeleteFileInput{
		Owner:            owner,
		RemotePath:       f.Filename,
		ExpectedChecksum: f.Checksum,
		AllowMissing:     true,
		SkipFileLock:     true,
	})
	if err != nil {
		result.Message = batchDeleteMessage(err)
		return result
	}
	result.Success = true
	if res.Idempotent {
		result.Message = res.Message // 「文件不存在（幂等删除）」
		return result
	}
	result.Message = "删除成功"
	return result
}

// batchDeleteMessage 把删除域错误映射为**批量族的历史文案**（仅三项与单条族不同；其余一致）。
// 按 `HTTPError.Reason`（稳定标识）分派，不比对中文。
func batchDeleteMessage(err error) string {
	var he *HTTPError
	if !errors.As(err, &he) {
		return "删除失败"
	}
	switch he.Reason {
	case reasonPathInvalid:
		return "无效的文件路径" // 单条族回 errMsgInvalidFilename
	case reasonChecksumMissing:
		return "缺少 checksum" // 单条族回 errMsgMissingChecksum
	case reasonRemoveFailed:
		return "删除失败" // 单条族回 "删除文件失败"
	default:
		return he.Message
	}
}

// BatchDelete 处理 POST /api/batch/delete。
// 请求体 JSON：{"files": [{"file_name": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余文件。
func (s *Service) BatchDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		s.sendJSON(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	owner := s.rt.actorOf(r)
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, s.processBatchDeleteItem(r.Context(), owner, f))
	}
	s.sendJSON(w, BatchResponse{Results: results}, http.StatusOK)
}
