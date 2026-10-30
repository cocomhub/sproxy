// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// s3_test_alias_test.go 是 S3 测试的**迁移期别名**：实现已下沉 pkg/server/s3，
// 测试保持既有可读名（后续把白盒用例整体搬入 s3 包时可删除本文件）。

import "github.com/cocomhub/sproxy/pkg/server/s3"

type (
	s3CompletePart       = s3.CompletePart
	s3CompleteSettlement = s3.CompleteSettlement
)

const (
	multipartPartPrefix  = s3.MultipartPartPrefix
	s3MaxParts           = s3.MaxParts
	s3MultipartOrphanTTL = s3.MultipartOrphanTTL
)

var (
	rootWriteFile         = s3.RootWriteFile
	readMultipartMeta     = s3.ReadMultipartMeta
	normalizeETag         = s3.NormalizeETag
	xmlEscapeText         = s3.XMLEscapeText
	validateCompleteParts = s3.ValidateCompleteParts
)

// sha256sum 是测试侧别名（实现已下沉 pkg/server/s3）。
func sha256sum(b []byte) []byte { return s3.SHA256Sum(b) }
