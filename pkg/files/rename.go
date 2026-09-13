// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rename.go 是重命名族的 **HTTP 面**：`POST /rename`（薄适配）与 `POST /api/batch/rename`
// 的 DTO 与逐条处理，自 `pkg/server/rename_handler.go` 整族迁入（接收者由 *Handlers 改为
// *Service）。
//
// 分工（D-2 第 4 片）：**领域逻辑在 write_ops.go 的 `RenameFile`**（跨卷定位、AD-4 唯一性、
// checksum 门禁、配额对称转移、审计），本文件只保留「解析请求 → 调域方法 → 写响应」与
// 批量族的逐条结果聚合。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// errMsgSrcChecksumFailed / errMsgCreateParentDirFailed 是重命名族的失败响应文案（随本族
// 自 pkg/server/errors.go 迁入；两个常量在 pkg/server 均已无其他使用者）。
const (
	errMsgSrcChecksumFailed     = "源文件 SHA-256 校验失败"
	errMsgCreateParentDirFailed = "目标路径父目录创建失败"
)

// BatchRenameRequest 批量重命名请求体。
type BatchRenameRequest struct {
	Operations []BatchRenameOp `json:"operations"`
}

// BatchRenameOp 单条重命名操作。
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}

// processBatchRenameItem 处理单条批量重命名操作：**在域方法之上**循环，只做「调用 → 文案映射
// → 结果聚合」，不再复制写语义（P2-c）。
//
// 语义与错误聚合逐字不变：路径校验、跨卷定位、AD-4 唯一性、checksum 门禁、配额对称转移、
// 原子改名、checksum 台账与审计**全部**由 RenameFile（内核 renameInHome）承担；本函数只把域
// 错误映射回批量族的历史文案（仅两项与单条族不同：缺 checksum、建父目录失败），并给出批量族
// 自己的成功文案「重命名成功」（同源同目标短路仍原样透传域文案）。
func (s *Service) processBatchRenameItem(ctx context.Context, owner string, op BatchRenameOp) BatchOperationResult {
	result := BatchOperationResult{Filename: op.From + " -> " + op.To}
	res, err := s.RenameFile(ctx, RenameFileInput{
		Owner:            owner,
		From:             op.From,
		To:               op.To,
		ExpectedChecksum: op.Checksum,
		Origin:           auditOriginBatch,
	})
	if err != nil {
		result.Message = batchRenameMessage(err)
		return result
	}
	result.Success = true
	if res.NoOp {
		result.Message = res.Message // 「源与目标相同，无需移动」
		return result
	}
	result.Message = "重命名成功"
	return result
}

// batchRenameMessage 把重命名域错误映射为**批量族的历史文案**（仅两项与单条族不同；其余
// 文案两族一致）。按 `HTTPError.Reason`（稳定标识）分派，不比对中文。
func batchRenameMessage(err error) string {
	var he *HTTPError
	if !errors.As(err, &he) {
		return "重命名失败"
	}
	switch he.Reason {
	case reasonChecksumMissing:
		return "缺少 checksum" // 单条族回 errMsgMissingChecksum
	case reasonMkdirFailed:
		return "创建父目录失败" // 单条族回 errMsgCreateParentDirFailed
	default:
		return he.Message
	}
}

// BatchRename 处理 POST /api/batch/rename。
// 请求体 JSON：{"operations": [{"from": "...", "to": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余操作。
func (s *Service) BatchRename(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.Operations) == 0 {
		s.sendJSON(w, UploadResponse{Success: false, Message: "operations 不能为空"}, http.StatusBadRequest)
		return
	}
	owner := s.rt.actorOf(r)
	results := make([]BatchOperationResult, 0, len(req.Operations))
	for _, op := range req.Operations {
		results = append(results, s.processBatchRenameItem(r.Context(), owner, op))
	}
	s.sendJSON(w, BatchResponse{Results: results}, http.StatusOK)
}

// Rename 处理 POST /rename?from=<old>&to=<new>。
// 与 delete 对称，要求 X-File-Checksum 头校验源文件，避免误覆盖。
// 目标路径已存在时返回 409；服务端会自动 mkdir -p 中间目录。
//
// 本处理器只做「解析三个入参（from/to/checksum + 可选 volume）→ 调域方法 → 写响应」；
// 全部领域逻辑（跨卷定位、AD-4 唯一性、checksum 门禁、配额转移、审计）在 write_ops.go
// 的 RenameFile 内（远程写面复用同一份语义，见 write_ops.go 顶注）。
func (s *Service) Rename(w http.ResponseWriter, r *http.Request) {
	res, err := s.RenameFile(r.Context(), RenameFileInput{
		Owner:            s.rt.actorOf(r),
		From:             r.URL.Query().Get("from"),
		To:               r.URL.Query().Get("to"),
		ExpectedChecksum: r.Header.Get(headerFileChecksum),
		ExplicitVol:      r.URL.Query().Get("volume"),
	})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: res.Message, Checksum: res.Checksum}, http.StatusOK)
}
