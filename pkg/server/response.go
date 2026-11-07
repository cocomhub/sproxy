// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"mime"
	"net/http"
	"net/url"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/server/cloudtasks"
)

// SetResponseLogger 设置 JSON 响应写出失败时的告警日志器（委托 internal/httpjson）。
func SetResponseLogger(l *slog.Logger) {
	httpjson.SetLogger(l)
}

// UploadResponse 是通用响应结构。
type UploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

// 分块族的响应 DTO（ChunkedInitResponse / ChunkStatusResponse / UploadSessionInfo /
// ChunkSessionsResponse / ChunkUploadResponse / ChunkCompleteResponse）随分块处理器迁入
// pkg/files（DTO 定义在写出它的包里，见该包 service.go 的 HTTP 契约小节）。

// sendJSONResponse 写 JSON 响应（Content-Type + 序列化 + 状态码 + 序列化失败兜底）。
//
// 实现已收敛到 internal/httpjson（全仓唯一实现）：此前 pkg/files 与
// pkg/server/ratelimit 各持一份等价副本，收敛后只保留这一处装配层入口。
func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	httpjson.Send(w, statusCode, response)
}

// formatContentDisposition 使用标准库安全地构造 Content-Disposition 头。
// 同时设置 filename（传统）和 filename*（RFC 5987）参数，以支持非 ASCII 文件名。
func formatContentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	return mime.FormatMediaType("attachment", map[string]string{
		"filename":  filename,
		"filename*": "UTF-8''" + url.PathEscape(filename),
	})
}

// 批量删除 / 批量重命名的请求与响应 DTO（BatchDeleteRequest / BatchDeleteFile /
// BatchRenameRequest / BatchRenameOp / BatchOperationResult / BatchResponse）随写面处理器
// 迁入 pkg/files（delete.go / rename.go / service.go）：它们在本层**已无非测试消费者**
// （唯一入口是已迁走的 /delete、/rename、/api/batch/* 四个端点），故定义随写出它的处理器走
// ——与只读面的 FileInfo / ListResponse 同一处置（类型同一 ⇒ 跨侧漂移结构性不可能）。
// 本包测试直接引用 `files.*`。

// CloudBatchTaskResult 批量下载单个任务结果。
//
// DTO 随写出它的处理器走（P1-c S2a 迁入 pkg/server/cloudtasks）；此处保留同名
// **类型别名**，让既有装配层测试与调用点零改动引到同一类型（同一语义只留一份定义）。
type CloudBatchTaskResult = cloudtasks.BatchTaskResult
