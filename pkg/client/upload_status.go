// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
)

// UploadStatus 是 GET /upload/status 的响应：分块上传会话的接收/写入进度。
//
// 字段与服务端 pkg/files.ChunkStatusResponse 逐字对应（客户端侧唯一实现）。
// 服务端阶段进度见 Stage / Encrypt* / Remote*（进度可视化二期 T4，可选、
// 向后兼容：旧服务端不返回这些字段 ⇒ 零值）。
type UploadStatus struct {
	Success       bool   `json:"success"`
	UploadID      string `json:"upload_id,omitempty"`
	ReceivedCount int    `json:"received_count,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	MissingChunks []int  `json:"missing_chunks,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
	FileChecksum  string `json:"file_checksum,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Message       string `json:"message,omitempty"`
	// —— 服务端写入阶段进度（T4，2026-11-10）——
	// Stage 为当前阶段名；Encrypt* 为「服务端加密」进度；Remote* 为「远端卷上传」进度。
	Stage        string `json:"stage,omitempty"`
	EncryptDone  int64  `json:"encrypt_done,omitempty"`
	EncryptTotal int64  `json:"encrypt_total,omitempty"`
	RemoteDone   int64  `json:"remote_done,omitempty"`
	RemoteTotal  int64  `json:"remote_total,omitempty"`
}

// UploadStatus 查询分块上传会话状态（GET /upload/status）。
//
// uploadID 与 filename 任一可为空（走 query，语义与服务端一致）。会话不存在时
// 服务端返回 HTTP 404 ⇒ 返回包装了 ErrNotFound 的错误，**不**静默返回零值成功。
// 响应体读取沿用客户端 1MiB 上限口径（与 chunked 上传路径一致），防超大响应。
func (c *FileClient) UploadStatus(ctx context.Context, uploadID, filename string) (*UploadStatus, error) {
	urlPath := fmt.Sprintf("/upload/status?upload_id=%s&filename=%s",
		url.QueryEscape(uploadID), url.QueryEscape(filename))
	resp, err := c.doRequest(ctx, "GET", urlPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("查询上传状态失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, doJSONStatusError(resp)
	}

	var st UploadStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("解析上传状态响应失败: %w", err)
	}
	return &st, nil
}
