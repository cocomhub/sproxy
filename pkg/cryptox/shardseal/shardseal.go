// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shardseal 实现随机分块 + AES-256-GCM + scrypt 派生的自描述加密分块算法
// （设计 docs/designs/2026-10-01-secret-volume.md）。对外 API：EncryptShards /
// DecryptFile；命名含三段 16hex 校验和截断 + 随机段，meta 含全 stat + 每分块 stat。
package shardseal

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBlockPolicy 返回默认分块策略（设计 §4.1：1MB-200MB 随机）。
func DefaultBlockPolicy() BlockPolicy {
	return BlockPolicy{Mode: "random", Min: 1 << 20, Max: 200 << 20}
}

// Planner 由 BlockPolicy 构造 BlockPlanner（当前仅 "random"；未知 mode fail-closed 返回 nil）。
func (p BlockPolicy) Planner() BlockPlanner {
	if p.Mode == "" || p.Mode == "random" {
		minSize, maxSize := p.Min, p.Max
		if minSize <= 0 {
			minSize = 1
		}
		if maxSize < minSize {
			maxSize = minSize
		}
		return &RandomPlanner{Min: minSize, Max: maxSize}
	}
	// 未知 mode（如 video-keyframe 尚未注册）：返回 nil，调用方 fail-closed。
	return nil
}

// EncryptShards 把本地文件加密为分块 + meta，返回分块与 meta 信息。
// srcFile：原始文件路径；outDir：加密分块输出目录；secret：密钥；policy：分块策略；
// padTarget：meta 加密 padding 目标（整块落盘总长，0=不 padding；secretdata 卷传
// min_block_size 附近值）。流程：读全文件 → 分块 → 每块 AES-256-GCM 加密（统一格式
// [R][4B 密文长][salt][nonce][ct+tag]）→ 写分块文件 → 生成 meta（全 stat + 每块
// stat）→ meta 明文整体加密到 padTarget 并落盘 → 返回包含最终 MetaBlob 的产物。
// 磁盘上不出现明文 meta JSON（含文件名/size/sha256），meta 名三段真实补齐并锚定
// 最终 blob（首段=明文哈希，中段=总校验和，末段=MetaBlob 哈希）。
func EncryptShards(srcFile, outDir string, secret []byte, policy BlockPolicy, padTarget int) (*EncryptionResult, error) {
	src, err := os.Open(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 打开源文件 %s 失败: %w", srcFile, err)
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 读源文件失败: %w", err)
	}
	st, err := os.Stat(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: stat 源文件失败: %w", err)
	}

	plan := policy.Planner()
	if plan == nil {
		return nil, fmt.Errorf("shardseal: 未知分块策略 %q", policy.Mode)
	}
	blocks, err := plan.Plan(int64(len(data)))
	if err != nil {
		return nil, err
	}

	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	key, err := deriveKey(secret, salt)
	if err != nil {
		return nil, err
	}

	totalHex, err := hash16(data)
	if err != nil {
		return nil, err
	}

	res := &EncryptionResult{Meta: &Meta{
		Version:   metaVersion,
		Algorithm: AlgorithmName,
		KDF:       "scrypt",
		Salt:      toBase64(salt),
		Original: OriginalInfo{
			Name:      filepath.Base(srcFile),
			Size:      st.Size(),
			SHA256:    sha256Hex64(data),
			MTime:     st.ModTime().UTC().Format(time.RFC3339Nano),
			Mode:      uint32(st.Mode().Perm()),
			MediaType: mediaType(filepath.Base(srcFile)),
		},
		Block: policy,
	}}

	names, chunkInfos, cerr := encryptWriteChunks(data, blocks, key, salt, totalHex, outDir)
	if cerr != nil {
		return nil, cerr
	}
	res.ChunkNames = names
	res.Meta.Chunks = chunkInfos

	metaName, merr := encryptWriteMeta(res, key, salt, totalHex, outDir, padTarget)
	if merr != nil {
		return nil, merr
	}
	res.MetaName = metaName
	return res, nil
}

// encryptWriteMeta 把 meta 明文 JSON 整体加密到 padTarget 落盘并返回 meta 文件名
// （EncryptShards 的 meta 处理，抽方法控制认知复杂度 #727 gocognit=15）。meta 明文 =
// [4B jsonLen][metaJSON][rand padding 到 padTarget]，整体加密为统一格式
// [R][4B 密文长][salt][nonce][ct+tag]。meta 名三段真实并锚定最终 blob：首段 = meta
// 明文哈希前 16，中段 = 原始总校验和前 16，末段 = MetaBlob 哈希前 16——磁盘上不出
// 现明文 meta JSON。
func encryptWriteMeta(res *EncryptionResult, key, salt []byte, totalHex, outDir string, padTarget int) (string, error) {
	metaJSON, err := json.Marshal(res.Meta)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 序列化失败: %w", err)
	}
	metaOrigHex, err := hash16(metaJSON)
	if err != nil {
		return "", err
	}
	metaBlob, err := encryptMetaJSON(key, salt, metaJSON, padTarget)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 加密失败: %w", err)
	}
	res.MetaBlob = metaBlob
	metaEncHex, err := hash16(metaBlob)
	if err != nil {
		return "", err
	}
	metaName := MetaName(metaOrigHex, totalHex, metaEncHex)
	if err := os.WriteFile(filepath.Join(outDir, metaName), metaBlob, 0o600); err != nil {
		return "", fmt.Errorf("shardseal: 写 meta %s 失败: %w", metaName, err)
	}
	return metaName, nil
}

// encryptWriteChunks 逐块加密并写盘，返回分块文件名与 ChunkInfo（EncryptShards 的
// 分块处理，抽方法控制认知复杂度 #727 gocognit=15）。Index 为 0 基顺序号。
func encryptWriteChunks(data []byte, blocks []Block, key, salt []byte, totalHex, outDir string) ([]string, []ChunkInfo, error) {
	var names []string
	var chunks []ChunkInfo
	for _, b := range blocks {
		chunk := data[b.Offset : b.Offset+b.Size]
		enc, cerr := encryptBlock(key, salt, chunk)
		if cerr != nil {
			return nil, nil, cerr
		}
		origBlockHex, _ := hash16(chunk)
		encBlockHex, _ := hash16(enc)
		name := ChunkName(origBlockHex, totalHex, encBlockHex)
		if werr := os.WriteFile(filepath.Join(outDir, name), enc, 0o600); werr != nil {
			return nil, nil, fmt.Errorf("shardseal: 写分块 %s 失败: %w", name, werr)
		}
		names = append(names, name)
		chunks = append(chunks, ChunkInfo{
			Index:      len(chunks),
			FileName:   name,
			OrigSize:   int64(len(chunk)),
			OrigSHA256: origBlockHex,
			EncSize:    int64(len(enc)),
			EncSHA256:  encBlockHex,
		})
	}
	return names, chunks, nil
}

// DecryptFile 用 meta + 分块还原原始文件到 dstFile。
// meta：meta 内容；chunkDir：分块所在目录；dstFile：还原目标；secret：密钥。
func DecryptFile(meta *Meta, chunkDir, dstFile string, secret []byte) error {
	if err := validateMeta(meta); err != nil {
		return err
	}
	// 每文件只派生一次 key（meta.Salt 是文件级盐；逐块重复 scrypt 在 N=2^17 下
	// 不可接受——S5344 提升参数后派生开销放大，整文件一次派生保持解密线性）。
	salt, err := decodeSalt(meta)
	if err != nil {
		return err
	}
	key, err := deriveKey(secret, salt)
	if err != nil {
		return err
	}
	mode := os.FileMode(meta.Original.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("shardseal: 创建还原文件 %s 失败: %w", dstFile, err)
	}
	full := sha256.New() // 还原同时累加整文件 SHA-256，用于 meta.Original.SHA256 全量校验
	if err := writeDecryptedChunks(f, meta, chunkDir, key, salt, full); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("shardseal: 关闭还原文件失败: %w", err)
	}
	// 设计 §5：meta.Original.SHA256 供解密后全量校验。只按等长逐块比对
	// 不够——等长交换/重排分块会静默产出错内容。此处对重组明文做整文件 SHA-256，
	// 与 meta 不一致即判失败（fail-closed，并删除残file）。
	if want := meta.Original.SHA256; want != "" {
		if hex.EncodeToString(full.Sum(nil)) != want {
			_ = os.Remove(dstFile)
			return fmt.Errorf("shardseal: 还原内容完整性校验失败（sha256 不匹配）")
		}
	}
	return nil
}

// decodeSalt 解码 meta 的文件级盐（meta.Salt 为 base64）。空/非法即失败（fail-closed）。
func decodeSalt(meta *Meta) ([]byte, error) {
	if meta == nil || meta.Salt == "" {
		return nil, fmt.Errorf("shardseal: meta 缺少 salt")
	}
	salt, err := base64.StdEncoding.DecodeString(meta.Salt)
	if err != nil {
		return nil, fmt.Errorf("shardseal: meta salt 解码失败: %w", err)
	}
	if len(salt) != SaltLen {
		return nil, fmt.Errorf("shardseal: meta salt 长度 %d，应为 %d", len(salt), SaltLen)
	}
	return salt, nil
}

// writeDecryptedChunks 逐块解密写入 f 并累加整文件 SHA-256（DecryptFile 的分块处理，
// 抽方法控制认知复杂度 #727 gocognit=15）。任何分块失败返回错误（f 由调用方 Close）。
// key 与 salt 是 DecryptFile 派生一次的文件密钥与文件级盐（decryptBlock 内做块内
// salt 一致性校验）。
func writeDecryptedChunks(f *os.File, meta *Meta, chunkDir string, key, salt []byte, full hash.Hash) error {
	for _, ci := range meta.Chunks {
		blob, err := os.ReadFile(filepath.Join(chunkDir, ci.FileName))
		if err != nil {
			return fmt.Errorf("shardseal: 读分块 %s 失败: %w", ci.FileName, err)
		}
		plain, err := decryptBlock(key, salt, blob)
		if err != nil {
			return err
		}
		if int64(len(plain)) != ci.OrigSize {
			return fmt.Errorf("shardseal: 分块 %s 解密长度 %d 不匹配 meta %d", ci.FileName, len(plain), ci.OrigSize)
		}
		if _, err := f.Write(plain); err != nil {
			return fmt.Errorf("shardseal: 写还原文件失败: %w", err)
		}
		full.Write(plain)
	}
	return nil
}

// mediaType 依据扩展名返回 media_type（meta 审计字段；未知返回空）。
func mediaType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".avi":
		return "video" + ext
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image" + ext
	case ".mp3", ".flac", ".wav":
		return "audio" + ext
	}
	return ""
}
