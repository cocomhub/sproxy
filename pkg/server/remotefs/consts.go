// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remotefs

// HTTP 头与响应文案常量：跨节点面自己的线上契约（消费者是 pkg/remote 客户端）。
// 与装配层同名常量刻意各持一份——它们是**跨进程协议**的字段名，不是内部共享配置；
// 绑定到装配根会让本包反向依赖（R6）。
const (
	headerContentType  = "Content-Type"
	headerFileChecksum = "X-File-Checksum"
	headerFileMTime    = "X-File-MTime"

	contentTypeJSON        = "application/json"
	contentTypeOctetStream = "application/octet-stream"

	msgNotFound = "not found"
)
