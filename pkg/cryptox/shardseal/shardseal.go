// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shardseal 实现随机分块 + AES-256-GCM + scrypt 派生的自描述加密分块算法
// （设计 docs/designs/2026-10-01-secret-volume.md）。对外 API：EncryptShards /
// DecryptFile；命名含三段 16hex 校验和截断 + 随机段，meta 含全 stat + 每分块 stat。
package shardseal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// EncryptShards 把本地文件加密为分块文件 + meta，返回分块与 meta 信息。
// srcFile：原始文件路径；outDir：加密分块输出目录；secret：密钥；policy：分块策略。
// 流程：读全文件 → 分块 → 每块 AES-256-GCM 加密（块自描述 [salt][nonce][ct+tag]）→
// 写分块文件 → 生成 meta（全 stat + 每块 stat）→ 写 meta 文件。
func EncryptShards(srcFile, outDir string, secret []byte, policy BlockPolicy) (*EncryptionResult, error) {
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

	for _, b := range blocks {
		chunk := data[b.Offset : b.Offset+b.Size]
		enc, cerr := encryptBlock(key, salt, chunk)
		if cerr != nil {
			return nil, cerr
		}
		origBlockHex, _ := hash16(chunk)
		encBlockHex, _ := hash16(enc)
		name := ChunkName(origBlockHex, totalHex, encBlockHex)
		if werr := os.WriteFile(filepath.Join(outDir, name), enc, 0o600); werr != nil {
			return nil, fmt.Errorf("shardseal: 写分块 %s 失败: %w", name, werr)
		}
		res.ChunkNames = append(res.ChunkNames, name)
		res.Meta.Chunks = append(res.Meta.Chunks, ChunkInfo{
			Index:      len(res.Meta.Chunks),
			FileName:   name,
			OrigSize:   int64(len(chunk)),
			OrigSHA256: origBlockHex,
			EncSize:    int64(len(enc)),
			EncSHA256:  encBlockHex,
		})
	}

	metaJSON, err := json.Marshal(res.Meta)
	if err != nil {
		return nil, fmt.Errorf("shardseal: meta 序列化失败: %w", err)
	}
	metaName := MetaName(totalHex)
	if err := os.WriteFile(filepath.Join(outDir, metaName), metaJSON, 0o600); err != nil {
		return nil, fmt.Errorf("shardseal: 写 meta %s 失败: %w", metaName, err)
	}
	res.MetaName = metaName
	res.Meta.MetaFileName = metaName
	return res, nil
}

// DecryptFile 用 meta + 分块还原原始文件到 dstFile。
// meta：meta 内容；chunkDir：分块所在目录；dstFile：还原目标；secret：密钥。
func DecryptFile(meta *Meta, chunkDir, dstFile string, secret []byte) error {
	if err := validateMeta(meta); err != nil {
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
	for _, ci := range meta.Chunks {
		blob, err := os.ReadFile(filepath.Join(chunkDir, ci.FileName))
		if err != nil {
			f.Close()
			return fmt.Errorf("shardseal: 读分块 %s 失败: %w", ci.FileName, err)
		}
		plain, err := decryptBlock(secret, blob)
		if err != nil {
			f.Close()
			return err
		}
		if int64(len(plain)) != ci.OrigSize {
			f.Close()
			return fmt.Errorf("shardseal: 分块 %s 解密长度 %d 不匹配 meta %d", ci.FileName, len(plain), ci.OrigSize)
		}
		if _, err := f.Write(plain); err != nil {
			f.Close()
			return fmt.Errorf("shardseal: 写还原文件失败: %w", err)
		}
		full.Write(plain)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("shardseal: 关闭还原文件失败: %w", err)
	}
	// 设计 §5：meta.Original.SHA256 供解密后全量校验。只按等长逐块比对
	// 不够——等长交换/重排分块会静默产出错内容。此处对重组明文做整文件 SHA-256，
	// 与 meta 不一致即判失败（fail-closed，并删除残file）。
	if want := meta.Original.SHA256; want != "" {
		if got := hex.EncodeToString(full.Sum(nil)); got != want {
			_ = os.Remove(dstFile)
			return fmt.Errorf("shardseal: 还原内容完整性校验失败（sha256 不匹配）")
		}
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
