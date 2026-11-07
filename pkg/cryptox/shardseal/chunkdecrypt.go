// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// chunkdecrypt.go 提供**分块来源可注入**的解密路径（2026-10-18 架构级修复 Step 1）。
//
// 为什么需要：`DecryptFile(meta, chunkDir, dstFile, secret)` 是「**本地目录 → 本地文件**」的
// 路径型 API ⇒ 调用方被迫先把**全部分块密文**落盘（secretdata 的读路径正是如此：先复制进
// `temp/view-*`，再解密整份明文 ⇒ 峰值临时盘 ≈ 2× 文件 ✗）。而解密所需的全部信息都在
// `ChunkInfo`（文件名/偏移/明文长度/块内 blocklet）里 ⇒ 密文**无需**先落盘，来源可以是底层
// FS、内存或远端。
//
// 与 `DecryptFile` 的关系（**唯一实现**，避免两套解密逻辑分叉）：
// `DecryptFile` = 本类型 + 「本地目录」来源 + 「本地文件」目标（对外行为与错误语义完全不变）；
// 需要不落密文临时文件的调用方直接注入自己的 `ChunkSource`。
//
// 保证（与 DecryptFile 逐条一致，不得削弱）：
//   - **逐块认证**：每块 GCM 认证解密（`decryptBlock`，块内 salt 一致性校验在内）；
//   - **逐块长度校验**：解密后明文长度必须等于 `ci.OrigSize`；
//   - **整文件校验**：全部块按 meta 顺序重组后做整文件 SHA-256，与 `meta.Original.SHA256`
//     比对（只按块比对不够——**等长交换/重排分块**会静默产出错内容）⇒ 不符返回
//     `ErrIntegrityMismatch`（调用方负责清理已写出的部分内容）。

// ErrIntegrityMismatch 是「还原内容与 meta 记录的整文件 SHA-256 不一致」哨兵（fail-closed）。
var ErrIntegrityMismatch = errors.New("shardseal: 还原内容完整性校验失败")

// ChunkSource 按 meta 的 `ChunkInfo` 返回该块的**密文 blob 字节**。
// 实现方自行决定来源（本地目录 / 底层 FS / 内存 / 远端），并自行决定是否需要 erasure 恢复；
// 返回的字节必须与写入时落盘的 blob 完全一致（分块自描述，解密侧校验）。
type ChunkSource func(ci ChunkInfo) ([]byte, error)

// ChunkDecryptor 是「**一次派生、逐块解密**」的解密器：持有文件级派生密钥与盐。
// 每文件只派生一次（`meta.Salt` 是文件级盐；逐块重复 scrypt 在 high 档下不可接受）。
type ChunkDecryptor struct {
	meta *Meta
	key  []byte
	salt []byte
}

// NewChunkDecryptor 校验 meta 并派生文件级密钥（失败即 fail-closed）。
func NewChunkDecryptor(secret []byte, meta *Meta) (*ChunkDecryptor, error) {
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	salt, err := decodeSalt(meta)
	if err != nil {
		return nil, err
	}
	key, err := DeriveKey(secret, salt, meta.AlgoVersion)
	if err != nil {
		return nil, err
	}
	return &ChunkDecryptor{meta: meta, key: key, salt: salt}, nil
}

// Meta 返回解密器绑定的 meta（只读用途，例如按 `Chunks[i].Offset` 做随机访问定位）。
func (d *ChunkDecryptor) Meta() *Meta { return d.meta }

// DecryptChunk 解密**单块**（GCM 认证 + 明文长度与 meta 一致校验），返回明文。
func (d *ChunkDecryptor) DecryptChunk(ci ChunkInfo, src ChunkSource) ([]byte, error) {
	blob, err := src(ci)
	if err != nil {
		return nil, err
	}
	plain, err := decryptBlock(d.key, d.salt, blob)
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) != ci.OrigSize {
		return nil, fmt.Errorf("shardseal: 分块 %s 解密长度 %d 不匹配 meta %d",
			ci.FileName, len(plain), ci.OrigSize)
	}
	return plain, nil
}

// DecryptAllTo 按 meta 顺序逐块解密写入 w，并对重组后的明文做**整文件 SHA-256** 校验。
// 失败时 w 可能已写入部分内容——清理由调用方负责（与 DecryptFile 的「删残件」同责）。
func (d *ChunkDecryptor) DecryptAllTo(w io.Writer, src ChunkSource) error {
	full := sha256.New()
	for _, ci := range d.meta.Chunks {
		plain, err := d.DecryptChunk(ci, src)
		if err != nil {
			return err
		}
		if _, err := w.Write(plain); err != nil {
			return fmt.Errorf("shardseal: 写还原内容失败: %w", err)
		}
		full.Write(plain)
	}
	if want := d.meta.Original.SHA256; want != "" {
		if hex.EncodeToString(full.Sum(nil)) != want {
			return fmt.Errorf("%w（sha256 不匹配）", ErrIntegrityMismatch)
		}
	}
	return nil
}
