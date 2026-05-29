// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestDecryptStream_ChunkTooLarge 构造一个 chunkLen 远超 maxChunkLen 的帧，
// 确认 DecryptStream 拒绝而不分配大内存。
func TestDecryptStream_ChunkTooLarge(t *testing.T) {
	key := make([]byte, 32) // 任意 256-bit key，反正解密会因长度先失败
	var attack bytes.Buffer
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 1<<24) // 16 MiB，远超 maxChunkLen (~64K+64)
	attack.Write(lenBuf)
	// 不需要真的写 16 MiB 数据：DecryptStream 应在 ReadFull 前的长度检查就 reject

	var out bytes.Buffer
	_, err := DecryptStream(key, &attack, &out)
	if err == nil {
		t.Fatalf("expected error for oversized chunk, got nil")
	}
	if !strings.Contains(err.Error(), "chunk too large") {
		t.Fatalf("expected 'chunk too large' error, got: %v", err)
	}
}

// TestDecryptStream_TruncatedFrame 长度后 body 截断，应返回 error 而非死循环。
func TestDecryptStream_TruncatedFrame(t *testing.T) {
	key := make([]byte, 32)
	var attack bytes.Buffer
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 100) // 声明 100B，但只写 10B
	attack.Write(lenBuf)
	attack.Write(make([]byte, 10))

	var out bytes.Buffer
	_, err := DecryptStream(key, &attack, &out)
	if err == nil {
		t.Fatalf("expected error for truncated frame, got nil")
	}
}
