// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// 通用错误消息常量（跨文件共享，避免字符串重复）
//
// 写面族专属的文案随处理器迁入 pkg/files：errMsgOpenFileFailed 在 chunked_response.go，
// errMsgMissingChecksum 在 write.go，
// errMsgSrcChecksumFailed / errMsgCreateParentDirFailed 在 rename.go——三者在本包均已无
// 消费者，故不留副本。headerFileMTime（X-File-MTime）同理：其在本包的最后一个消费者
// （upload 读客户端上报的 mtime）已随上传族迁走，常量归 pkg/files（read.go），本包不留。
const (
	errMsgEmptyFilename      = "文件名不能为空"
	errMsgInvalidFilename    = "无效的文件名"
	errMsgFileNotFound       = "文件不存在"
	errMsgInvalidPath        = "无效的文件路径"
	errMsgSaveFailed         = "保存文件失败"
	errMsgHubNotEnabled      = "hub 未启用"
	errMsgVersioningDisabled = "版本管理未启用"

	// HTTP 头常量
	//
	// 这些头名是**跨端契约**（pkg/client 与 pkg/remote 各自持有同名常量；A 侧按字面读取），
	// 故本包保留自己的副本——与"每端自持协议常量"的既有做法一致。
	headerContentType  = "Content-Type"
	headerFileChecksum = "X-File-Checksum"
	headerFileMTime    = "X-File-MTime" // stat/download 的修改时间（UnixNano）
	// headerContentDisposition 是下载响应文件名头（archive 下载共用）。
	headerContentDisposition = "Content-Disposition"

	// Content-Type 值常量
	contentTypeJSON        = "application/json"
	contentTypeOctetStream = "application/octet-stream"
	contentTypeTextPlain   = "text/plain; charset=utf-8"

	// 时间布局与存储/URL 路径前缀（S1192：跨 handler 共享，防拼写漂移）
	timeLayoutDate    = "2006-01-02"
	sharePrefix       = "share/"
	chunkPrefix       = "chunk/"
	partSuffix        = ".part." // s3 分块文件后缀（<key>.part.<N>）
	tarGZExt          = ".tar.gz"
	metaAIDir         = "meta/ai/"
	fileNameCredStore = "credentials.json"
	fileNameDedup     = "dedup.json"

	// 高频错误消息（跨 handler 共享文案）
	msgNotFound             = "not found"
	msgInvalidRequestBody   = "invalid request body"
	msgBadRequest           = "请求体校验失败"
	msgCredRingMissing      = "凭据 Ring 未装配"
	msgArchiveDirFail       = "failed to create archive directory"
	msgArchiveStatFail      = "failed to stat source file"
	msgGroupNotFound        = "group not found"
	msgJSONEncode           = "JSON encode error"
	msgRateLimitExceeded    = "rate limit exceeded"
	msgShareInvalid         = "分享链接无效或已过期"
	msgCredGenFailed        = "生成凭据失败"
	msgNonceMissing         = "nonce 池未装配"
	msgAdminLoopbackOnly    = "首个 admin 注册仅限回环"
	msgStorageQuotaExceeded = "存储配额不足"
	msgVolumeNotAllowed     = "volume not allowed"
	msgMoveFileFailed       = "移动文件失败"
	msgVersionFileMissing   = "版本文件不存在"
	msgVersionFileMissingPF = "版本文件不存在: "

	// S3 端点错误消息
	msgS3VolumeUnavailable = "s3: 卷不可用"
	msgS3Unauth            = "s3: 未认证"
	msgS3AuthFailed        = "s3: 认证失败"

	// routeVolumesBase 是卷管理路由前缀（注册列表与 HasPrefix 校验共享）。
	routeVolumesBase = "/api/volumes"
)
