// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"fmt"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

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
// 规则（2026-10-04 用户裁决 + 配置开关）：
//   - **显式配置优先**：policy.BlockletMode 非空（extra.block_policy.blocklet_mode 配置了
//     BlockletModeFixed/BlockletModeVideoKeyframe）→ 尊重显式配置；BlockletModeVideoKeyframe
//     仍需经注册表解析 Indexer（blockletPlanner 依赖它构造，缺则 fail-closed）；
//     BlockletModeFixed 直接返回（不选型）。
//   - 非视频扩展名 → 保持默认策略（全部未命中 → 默认分块策略）。
//   - 视频扩展名 → ResolveBlockletMode(kind)：
//     ErrPlannerConflict（同 Kind 多异名提供者）→ 返回错误（绑定失败，写路径 fail-closed）；
//     未命中（0 条）→ 默认 fixed；
//     命中唯一 video-keyframe → policy.BlockletMode=BlockletModeVideoKeyframe + 注入 Indexer。
//
// 返回 policy 副本（不改共享 s.opts.Block），写路径按文件独立选型。
//
// **分块规则引擎（2026-11-26）**：进入既有 blocklet 选型之前，先按「用户可见相对路径 + 体积」
// 解析规则命中（见 block_policy.go）——规则只覆盖**块级**（Mode/Max），blocklet 仍由下面的
// 扩展名自动选型/显式配置决定。未命中任何规则时返回卷默认策略，行为与产物逐字节不变。
func (s *SecretdataFS) blockletPolicyFor(name string, size int64) (shardseal.BlockPolicy, error) {
	p, matched, err := s.resolveBlockletPolicy(name, size)
	if err == nil {
		s.reportBlockMode(name, p, matched)
	}
	return p, err
}

// resolveBlockletPolicy 是 blockletPolicyFor 的实现体：解析块级规则 + blocklet 选型，
// 返回 (策略, 是否规则命中, error)。上报统一由 blockletPolicyFor 在**所有返回路径**上做
// （含显式配置早退、错误路径），避免「某些路径不上报」造成排障盲区。
func (s *SecretdataFS) resolveBlockletPolicy(name string, size int64) (shardseal.BlockPolicy, bool, error) {
	policy, matched := s.ruleForWrite(name, size)
	// 显式配置的 blocklet_mode（开关）优先。
	if policy.BlockletMode != "" {
		if policy.BlockletMode != shardseal.BlockletModeVideoKeyframe {
			return policy, matched, nil // 显式 "fixed" 或其它：尊重，不自动选型。
		}
		// 显式 video-keyframe：仍需注册表解析 Indexer（blockletPlanner 依赖）。
		p, err := s.injectKeyframeIndexer(policy, name)
		return p, matched, err
	}
	kind := shardseal.MediaKindOf(name)
	if kind != "" {
		reg, err := shardseal.ResolveBlockletMode(kind)
		if err != nil {
			return policy, matched, err
		}
		if reg.Mode == shardseal.BlockletModeVideoKeyframe && reg.Indexer != nil {
			policy.BlockletMode = shardseal.BlockletModeVideoKeyframe
			policy.Indexer = reg.Indexer
			policy.Fallback = reg.Fallback // 主解析失败时兜底（go-mp4 → ffprobe）
			return policy, matched, nil
		}
	}
	// 非视频 / 未命中 → 默认 fixed（归一化空值，meta 记录明确）。
	if policy.BlockletMode == "" {
		policy.BlockletMode = shardseal.BlockletModeFixed
	}
	return policy, matched, nil
}

// reportBlockMode 上报写路径最终确定的块级模式（nil 回调 = 不上报，零回归）。
// 命中规则与未命中都上报——「未命中」本身是排障时最需要的信息。
func (s *SecretdataFS) reportBlockMode(rel string, p shardseal.BlockPolicy, matched bool) {
	if s.opts.OnBlockModeResolved == nil {
		return
	}
	s.opts.OnBlockModeResolved(rel, p.Mode, matched)
}

// injectKeyframeIndexer 为显式 video-keyframe 配置注入 Indexer：按文件名 Kind（或回退
// 任一 video-keyframe 提供者）解析注册表；未命中返回错误（显式开启但无解析器 = fail-closed，
// 不静默降级）。
func (s *SecretdataFS) injectKeyframeIndexer(policy shardseal.BlockPolicy, name string) (shardseal.BlockPolicy, error) {
	// 优先按文件名 Kind 精确/通配匹配；非视频扩展名回退任一已注册 video-keyframe 提供者。
	kind := shardseal.MediaKindOf(name)
	if kind == "" {
		reg, rerr := shardseal.ResolveBlockletMode("video/mp4")
		if rerr == nil && reg.Mode == shardseal.BlockletModeVideoKeyframe && reg.Indexer != nil {
			policy.Indexer = reg.Indexer
			policy.Fallback = reg.Fallback
			return policy, nil
		}
		return policy, fmt.Errorf("secretdata: 显式 video-keyframe 但无已注册解析器（未装配 keyframe 提供者）")
	}
	reg, rerr := shardseal.ResolveBlockletMode(kind)
	if rerr != nil {
		return policy, rerr
	}
	if reg.Mode != shardseal.BlockletModeVideoKeyframe || reg.Indexer == nil {
		return policy, fmt.Errorf("secretdata: 显式 video-keyframe 但容器族 %q 无解析器", kind)
	}
	policy.Indexer = reg.Indexer
	policy.Fallback = reg.Fallback
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
