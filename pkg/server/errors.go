// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// 通用错误消息常量（跨文件共享，避免字符串重复）
const (
	errMsgEmptyFilename         = "文件名不能为空"
	errMsgInvalidFilename       = "无效的文件名"
	errMsgFileNotFound          = "文件不存在"
	errMsgInvalidPath           = "无效的文件路径"
	errMsgCreateDirFailed       = "创建目录失败"
	errMsgSaveFailed            = "保存文件失败"
	errMsgOpenFileFailed        = "打开文件失败"
	errMsgMissingChecksum       = "缺少 X-File-Checksum 请求头"
	errMsgFileReadFailed        = "文件读取失败"
	errMsgUploadIDNotFound      = "upload_id 不存在或已过期"
	errMsgHubNotEnabled         = "hub 未启用"
	errMsgCreateParentDirFailed = "目标路径父目录创建失败"
	errMsgVersioningDisabled    = "版本管理未启用"
	errMsgSrcChecksumFailed     = "源文件 SHA-256 校验失败"

	// HTTP 头常量
	headerContentType  = "Content-Type"
	headerFileChecksum = "X-File-Checksum"
	headerFileMTime    = "X-File-MTime"
	headerRequestID    = "X-Request-ID"

	// Content-Type 值常量
	contentTypeJSON        = "application/json"
	contentTypeOctetStream = "application/octet-stream"
	contentTypeTextPlain   = "text/plain; charset=utf-8"

	errFmtFileExists = "文件已存在，大小: %d"
)
