// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"io"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// cleanupReadCloser 是解密读的 ReadCloser：Close 时同时删除临时解密文件。
type cleanupReadCloser struct {
	rc   io.ReadCloser
	path string
}

func (c *cleanupReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *cleanupReadCloser) Close() error {
	err := c.rc.Close()
	_ = os.Remove(c.path)
	return err
}

// encryptContent 把明文内容分块加密写到 outDir，返回分块与 meta 信息。
// 走 shardseal.EncryptShardsBytes（内存变体，避免写临时源 + 二次整读——Imp-2 部分修复：
// secretdata 写路径已在 writeFile io.ReadAll 持有全文，直接复用该内存而非再落盘回读；
// 峰值从 2× 文件降到 1×）。origName 即逻辑文件名（EncryptShardsBytes 写入
// meta.original.Name——旧临时源路径以 sanitizeName 命名、EncryptShards 取 filepath.Base，
// 语义一致）。padTarget 透传给 EncryptShards（文件 meta 加密 padding 目标整块落盘总长；
// secretdata 卷传 metaPadTarget）。v 是算法版本（NewFS 由 Options.Algorithm 解析出的
// 已注册版本，写路径不硬编码）。
func encryptContent(data []byte, outDir string, secret []byte, policy shardseal.BlockPolicy, name string, padTarget int, v shardseal.AlgoVersion) (*shardseal.EncryptionResult, error) {
	return shardseal.EncryptShardsBytes(data, sanitizeName(name), outDir, secret, policy, padTarget, v)
}

// blockletPolicyFor 按文件名自动选型 blocklet 模式（video-keyframe 分块支持）。
// 规则（2026-10-04 用户裁决，pkg/plugin 注册表语义）：
//   - 非视频扩展名 → 保持默认策略（全部未命中 → 默认分块策略）。
//   - 视频扩展名 → ResolveBlockletMode(kind)：
//     ErrPlannerConflict（同 Kind 多异名提供者）→ 返回错误（绑定失败，写路径 fail-closed）；
//     未命中（0 条）→ 默认 fixed；
//     命中唯一 video-keyframe → policy.BlockletMode=video-keyframe + 注入 Indexer。
//
// 返回 policy 副本（不改共享 s.opts.Block），写路径按文件独立选型。
func (s *SecretdataFS) blockletPolicyFor(name string) (shardseal.BlockPolicy, error) {
	policy := s.opts.Block
	kind := shardseal.MediaKindOf(name)
	if kind != "" {
		reg, err := shardseal.ResolveBlockletMode(kind)
		if err != nil {
			return policy, err
		}
		if reg.Mode == "video-keyframe" && reg.Indexer != nil {
			policy.BlockletMode = "video-keyframe"
			policy.Indexer = reg.Indexer
			return policy, nil
		}
	}
	// 非视频 / 未命中 → 默认 fixed（归一化空值，meta 记录明确）。
	if policy.BlockletMode == "" {
		policy.BlockletMode = "fixed"
	}
	return policy, nil
}

// sanitizeName 把逻辑文件名归一为安全文件名（提供 meta.original.name / origName）。
func sanitizeName(name string) string {
	base := filepath.Base(name)
	if base == "." || base == "" {
		return "file"
	}
	return base
}
