// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/volume/ext/s3 是 S3 对象存储（可插拔扩展独立 module，仿 xfer/ext 模式）（AWS S3 / MinIO / COS / OSS 等兼容服务）后端的独立 Go module。
//
// 方案：**第三方 SDK（minio-go）隔离在独立 module**（仿 pkg/baidupcs 模式）——
// 主仓 go.mod 不直接依赖 minio-go，避免第三方依赖污染主仓依赖树；本 module 只含
// 自有代码（S3FS sync.FS 适配 + backend 注册），经 replace 指令接入主仓。

module github.com/cocomhub/sproxy/pkg/volume/ext/s3

go 1.27

replace github.com/cocomhub/sproxy => ../../../..

require (
	github.com/cocomhub/sproxy v0.0.0
	github.com/minio/minio-go/v7 v7.3.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
)
