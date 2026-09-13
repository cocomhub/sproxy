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

	// Content-Type 值常量
	contentTypeJSON        = "application/json"
	contentTypeOctetStream = "application/octet-stream"
	contentTypeTextPlain   = "text/plain; charset=utf-8"
)
