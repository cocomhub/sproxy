// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// 本包私有的协议/布局常量（原定义在 pkg/server/errors.go，唯一消费者是本网关）。
const (
	headerContentType = "Content-Type"

	// S3 端点错误消息。
	msgS3VolumeUnavailable = "s3: 卷不可用"
	msgS3Unauth            = "s3: 未认证"
	msgS3AuthFailed        = "s3: 认证失败"
)
