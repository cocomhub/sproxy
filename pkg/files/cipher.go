// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// cipher.go 是加密归档插件化（roadmap P2 加密归档插件化）：
//
//   - RegisterCipher 注册加密算法（分块大小）；LookupCipher 按名查。
//   - NewCipherWriter/NewCipherReader：流式 AES-256-GCM 分块加密/解密。
//   - **帧格式的唯一实现在 `pkg/cipherstream`**（头 [magic][4B 块大小] + 每块
//     [4B 长度][nonce|ct|tag]）；本包只提供算法注册表 + 魔数/块大小的域参数。
//     加密卷（pkg/sync）与客户端 E2EE（cmd/sclient）复用同一实现，仅魔数不同。
//
// 纯标准库（crypto/aes + crypto/cipher + crypto/rand），不引第三方。

import (
	"fmt"
	"io"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cipherstream"
)

// CipherConfig 是加密算法配置（LookupCipher 返回）。
type CipherConfig struct {
	// ChunkSize 是分块加密的明文块大小（64KiB 默认；GCM 认证粒度）。
	ChunkSize int
}

// cipherRegistry 是加密算法注册表（算法名 → 配置），与 transformRegistry 同构。
var cipherRegistry = struct {
	mu sync.RWMutex
	m  map[string]CipherConfig
}{m: make(map[string]CipherConfig)}

func init() {
	// 内建 AES-256-GCM（64KiB 分块）。
	RegisterCipher("aes-256-gcm", 64<<10)
}

// RegisterCipher 注册加密算法（算法名 → 分块大小）。返回 true = 新注册。
func RegisterCipher(name string, chunkSize int) bool {
	if name == "" || chunkSize <= 0 {
		return false
	}
	cipherRegistry.mu.Lock()
	defer cipherRegistry.mu.Unlock()
	if _, ok := cipherRegistry.m[name]; ok {
		return false
	}
	cipherRegistry.m[name] = CipherConfig{ChunkSize: chunkSize}
	return true
}

// LookupCipher 按算法名查询配置（ok=false = 未注册）。
func LookupCipher(name string) (CipherConfig, bool) {
	cipherRegistry.mu.RLock()
	defer cipherRegistry.mu.RUnlock()
	cfg, ok := cipherRegistry.m[name]
	return cfg, ok
}

// cipherMagic 是加密流的魔数头（防误读明文/其它格式）。
var cipherMagic = []byte("SPROXY-AES-GCM-v1")

// NewCipherWriter 构造流式加密 writer（分块 AES-256-GCM）。
//
// 帧格式与其它加密流（加密卷 / 客户端 E2EE）**完全一致**，仅魔数不同（域分隔）；
// 唯一实现在 pkg/cipherstream（三个使用方此前各写一份逐字相同的代码）。
func NewCipherWriter(algo string, key []byte, w io.Writer) (io.WriteCloser, error) {
	cfg, ok := LookupCipher(algo)
	if !ok {
		return nil, fmt.Errorf("cipher: 未知算法 %q", algo)
	}
	return cipherstream.NewWriter(key, w, cipherstream.Config{
		Magic: cipherMagic, ChunkSize: cfg.ChunkSize, VerifyChunkSize: true, Label: "cipher",
	})
}

// NewCipherReader 构造流式解密 reader（读 NewCipherWriter 产物）。
func NewCipherReader(algo string, key []byte, r io.Reader) (io.Reader, error) {
	cfg, ok := LookupCipher(algo)
	if !ok {
		return nil, fmt.Errorf("cipher: 未知算法 %q", algo)
	}
	return cipherstream.NewReader(key, r, cipherstream.Config{
		Magic: cipherMagic, ChunkSize: cfg.ChunkSize, VerifyChunkSize: true, Label: "cipher",
	})
}
