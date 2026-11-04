// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package e2ee 是 sclient 客户端 E2EE（roadmap P2 at-rest 加密残余）：
// 零知识——上传前 AES-256-GCM 流式加密（64KiB 块 + 每块随机 nonce + 魔数头），
// 下载后解密。帧格式由 `pkg/cipherstream` 统一实现。
package e2ee

import (
	"io"

	"github.com/cocomhub/sproxy/pkg/cipherstream"
)

// Magic 是客户端 E2EE 的加密流魔数头。
//
// 与服务端 at-rest 加密卷、归档 cipher 的魔数**不同是刻意的**：三者是不同密钥域，
// 客户端密文服务端不该能解（零知识），反之亦然。
var Magic = []byte("SPROXY-CIPHER-v1")

// NewEncryptWriter 返回流式加密 writer（写完需 Close 冲刷最后一块）。
//
// 帧格式的唯一实现在 `pkg/cipherstream`（服务端加密卷、归档 cipher 与客户端 E2EE 共用）；
// 本包只固定**客户端 E2EE 的魔数**——它是独立密钥域：
//   - 服务端**不能**解客户端密文（零知识），客户端也**不能**解服务端 at-rest 密文；
//   - 因此两处魔数不同是**有意的域分隔**，不是格式不一致。
//
// 历史：本包曾复制一份逐字相同的实现并注释「与 pkg/files cipher 同值互通」——那句注释是
// 错的（魔数不同，互不可解）；cmd/sclient 是独立 module 但**可以 import 根模块 pkg/**，
// 所以正确做法是用共享实现，而不是复制后用门禁锁一致。
func NewEncryptWriter(key []byte, w io.Writer) (io.WriteCloser, error) {
	return cipherstream.NewWriter(key, w, cipherstream.Config{
		Magic: Magic, ChunkSize: 64 << 10, Label: "e2ee",
	})
}

// NewDecryptReader 返回流式解密 reader（读 NewEncryptWriter 产物）。
func NewDecryptReader(key []byte, r io.Reader) (io.Reader, error) {
	return cipherstream.NewReader(key, r, cipherstream.Config{
		Magic: Magic, ChunkSize: 64 << 10, Label: "e2ee",
	})
}
