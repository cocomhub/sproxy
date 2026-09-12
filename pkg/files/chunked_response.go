// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"mime"
	"net/url"
)

// ---- 分块族 HTTP 契约（DTO / 请求体 / 响应头工具 / 错误文案） ----
//
// 本文件是分块族的 HTTP 契约归口（响应 DTO / 请求体 / 响应头工具 / 错误文案），按**族**分文件；
// 原 `pkg/files/chunked/response.go` 平铺到此，其中子包自有的 `UploadResponse` 副本已删除——
// 通用外壳只有一份定义，在 `service.go`。
//
// 全部字段名/顺序/omitempty 与迁移前逐字一致——响应体不得变化；两侧（pkg/server 与
// pkg/files）的 JSON 形状由 `pkg/server/response_drift_test.go` 与
// `pkg/server/chunked_wire_drift_test.go` 逐字节守卫。

// ChunkedInitRequest 是分块上传初始化请求体（POST /upload/init）。
//
// 从上传处理器的匿名解析结构提为**具名类型**：本族的请求契约此前两侧都是匿名结构
// （本包匿名 struct + SDK 未导出 chunkedInitRequest），无法反射比对；具名后
// `pkg/server/chunked_wire_drift_test.go` 才能把「服务端解析 ↔ SDK 构造 ↔ JS 构造」
// 三方钉在同一份字段/tag 上。字段名、顺序、tag 与提升前逐字一致（请求体契约不变）。
type ChunkedInitRequest struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	TotalSize    int64  `json:"total_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileChecksum string `json:"file_checksum"`
	FileModTime  int64  `json:"file_mod_time"`
	Volume       string `json:"volume,omitempty"` // 显式目标卷（可选；缺省 auto 路由）
}

// ChunkedCompleteRequest 是分块上传合并完成请求体（POST /upload/complete）。
// 具名理由同 ChunkedInitRequest；字段/tag 与提升前逐字一致。
type ChunkedCompleteRequest struct {
	UploadID string `json:"upload_id"`
}

// ChunkedInitResponse 分块上传初始化响应。
type ChunkedInitResponse struct {
	Success   bool   `json:"success"`
	UploadID  string `json:"upload_id,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ChunkStatusResponse 分块上传状态查询响应。
type ChunkStatusResponse struct {
	Success       bool   `json:"success"`
	UploadID      string `json:"upload_id,omitempty"`
	ReceivedCount int    `json:"received_count,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	MissingChunks []int  `json:"missing_chunks,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
	FileChecksum  string `json:"file_checksum,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Message       string `json:"message,omitempty"`
}

// UploadSessionInfo 是 GET /upload/sessions 列表中单个会话的信息条目。
type UploadSessionInfo struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	TotalSize     int64  `json:"total_size"`
	ReceivedCount int    `json:"received_count"`
	TotalChunks   int    `json:"total_chunks"`
	FileChecksum  string `json:"file_checksum"`
	FileModTime   int64  `json:"file_mod_time"` // UnixNano, 0 = unknown
	Status        string `json:"status"`        // uploading（Completed 会话被 handler 过滤，永不返回）
}

// ChunkSessionsResponse 是 GET /upload/sessions 的响应结构。
// Sessions 永远序列化为数组（不省略为 null），便于前端遍历。
type ChunkSessionsResponse struct {
	Success  bool                `json:"success"`
	Message  string              `json:"message,omitempty"`
	Sessions []UploadSessionInfo `json:"sessions"`
}

// ChunkUploadResponse 单块上传响应。
type ChunkUploadResponse struct {
	Success     bool   `json:"success"`
	ChunkIndex  int    `json:"chunk_index"`
	ShouldRetry bool   `json:"should_retry,omitempty"`
	Message     string `json:"message,omitempty"`
}

// ChunkCompleteResponse 分块上传合并完成响应。
type ChunkCompleteResponse struct {
	Success      bool   `json:"success"`
	Filename     string `json:"filename,omitempty"`
	FileChecksum string `json:"file_checksum,omitempty"`
	Message      string `json:"message,omitempty"`
	// MismatchChunks 是全文件校验失败（temp 名内容与 file_checksum 不符）时逐分片 seek
	// 重算后的坏分片索引列表（升序）。客户端据此只重传这些分片再 complete；重传后再次
	// complete 仍失败则继续收到更新后的列表。空（nil/无此字段）表示非 mismatch 类失败。
	MismatchChunks []int `json:"mismatch_chunks,omitempty"`
}

// formatContentDisposition 使用标准库安全地构造 Content-Disposition 头。
// 同时设置 filename（传统）和 filename*（RFC 5987）参数，以支持非 ASCII 文件名。
//
// 与 pkg/server.formatContentDisposition 同实现（该函数在 pkg/server 侧另有 4 个消费者，
// 无法随本族迁走，故此处为等价本地实现——见包文档「跨族共享的纯函数」）。
func formatContentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	return mime.FormatMediaType("attachment", map[string]string{
		"filename":  filename,
		"filename*": "UTF-8''" + url.PathEscape(filename),
	})
}

// 错误文案与协议常量。与 pkg/server/errors.go 中同名常量的字面量一致——其中多数在
// pkg/server 侧另有消费者（无法随本族删走），故此处是等价副本（只影响文案，不破 JSON 契约；
// 迁移期多份文案的收敛记录在任务 5/6 报告的「后续项」）。
const (
	errMsgInvalidFilename  = "无效的文件名"
	errMsgFileNotFound     = "文件不存在"
	errMsgInvalidPath      = "无效的文件路径"
	errMsgOpenFileFailed   = "打开文件失败"
	errMsgFileReadFailed   = "文件读取失败"
	errMsgUploadIDNotFound = "upload_id 不存在或已过期"
	errMsgSaveFailed       = "保存文件失败"
	errFmtFileExists       = "文件已存在，大小: %d"

	headerContentType      = "Content-Type"
	headerFileChecksum     = "X-File-Checksum"
	contentTypeJSON        = "application/json"
	contentTypeOctetStream = "application/octet-stream"
)
