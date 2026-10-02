// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import "fmt"

// AlgorithmName 是算法标识（写进 meta.algorithm）。
const AlgorithmName = "shardseal/aes-256-gcm"

// metaVersion 是 meta 结构版本。
const metaVersion = 1

// OriginalInfo 是原始文件的全 stat（设计 §5：全 stat + 每分块 stat，支持旧卷还原时
// 恢复元信息、无需重下载）。
type OriginalInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 是原始文件整文件 SHA-256（64 hex）。解密后可做全量校验。
	SHA256    string `json:"sha256"`
	MTime     string `json:"mtime,omitempty"`
	CTime     string `json:"ctime,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

// ChunkInfo 是单个分块的 stat（每块原始/加密名称与校验和、大小、nonce）。
type ChunkInfo struct {
	Index int `json:"index"`
	// FileName 是加密分块文件名（三段 16hex 命名）。
	FileName string `json:"file_name"`
	// Offset 是分块在原始文件中的字节偏移（0-based；随机访问定位用）。
	Offset int64 `json:"offset"`
	// OrigSize 是块的原始明文大小。
	OrigSize int64 `json:"orig_size"`
	// OrigSHA256 是块原始内容 SHA-256 前 16 hex。
	OrigSHA256 string `json:"orig_sha256"`
	// EncSize 是块的密文大小。
	EncSize int64 `json:"enc_size"`
	// EncSHA256 是块密文内容 SHA-256 前 16 hex（可校验密文完整）。
	EncSHA256 string `json:"enc_sha256"`
	// Nonce 是块 nonce（base64；当前块内联 nonce，字段为向后兼容/审计）。
	Nonce string `json:"nonce,omitempty"`
}

// BlockPolicy 是 meta 内记录的分块策略（设计 §2.2）。
type BlockPolicy struct {
	Mode string `json:"mode"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

// Meta 是文件级元数据（JSON 编解码）。
type Meta struct {
	Version   int          `json:"version"`
	Algorithm string       `json:"algorithm"`
	KDF       string       `json:"kdf"`
	SecretURL string       `json:"secret_url,omitempty"`
	Salt      string       `json:"salt"`
	Original  OriginalInfo `json:"original"`
	Chunks    []ChunkInfo  `json:"chunks"`
	// BlockPolicy 是生成时的分块策略（还原不依赖；旧卷读取审计用）。
	Block BlockPolicy `json:"block_policy"`
	// MetaFileName 是 meta 文件自身的命名（设计 §5）。
	MetaFileName string `json:"meta_file_name,omitempty"`
}

// EncryptionResult 是 EncryptShards 的产物：分块文件名 + 最终 meta blob + meta 文件名 + meta 内容。
type EncryptionResult struct {
	// ChunkNames 是加密分块文件名列表（写入 outDir）。
	ChunkNames []string
	// MetaName 是 meta 文件名（写入 outDir）。三段哈希锚定 MetaBlob：首段 =
	// hash16(metaJSON)、中段 = 原始总校验和前 16、末段 = hash16(MetaBlob)。
	MetaName string
	// MetaBlob 是最终可上传的加密 meta（含 padding），与落盘文件一致。
	MetaBlob []byte
	// Meta 是完整 meta（含全 stat + 每块 stat）。
	Meta *Meta
}

// validateMeta 校验 meta 字段完备性（失败 = 无法还原）。
func validateMeta(m *Meta) error {
	if m == nil {
		return fmt.Errorf("shardseal: meta 为 nil")
	}
	switch {
	case m.Version != metaVersion:
		return fmt.Errorf("shardseal: 未知 meta 版本 %d", m.Version)
	case m.Algorithm != AlgorithmName:
		return fmt.Errorf("shardseal: 未知算法 %q", m.Algorithm)
	case m.Original.Name == "" || m.Original.Size < 0:
		return fmt.Errorf("shardseal: meta 原始信息缺失（name=%q size=%d）", m.Original.Name, m.Original.Size)
	case len(m.Chunks) == 0:
		return fmt.Errorf("shardseal: meta 无分块")
	default:
	}
	for _, c := range m.Chunks {
		if c.FileName == "" || c.OrigSize < 0 {
			return fmt.Errorf("shardseal: meta 分块信息缺失（index=%d）", c.Index)
		}
	}
	return nil
}
