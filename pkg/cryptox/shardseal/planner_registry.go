// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

// KeyframeRequest 是关键帧解析的输入（Path 与 Reader 至少一个可用，调用方按使用方
// 能力传：secretdata 写路径持内存明文 → 传 Reader；本地文件上传可传 Path）。实现方
// 根据自身能力选择最优（ffprobe 有 Path 用文件路径避免 stdin 大文件降级；go-mp4 内存
// 解析 Reader 够用）。
type KeyframeRequest struct {
	// Path 是原始文件路径（可选；ffprobe 等子进程实现优先用文件路径）。
	Path string
	// Reader 是整文件随机读（可选；Path 为空时必传）。
	Reader io.ReaderAt
	// Size 是文件字节大小。
	Size int64
}

// KeyframeIndexer 是视频关键帧解析接口（可插拔，由 pkg/media/ext/mp4 等子模块实现）。
// 返回关键帧在原始文件中的**绝对字节偏移**（升序）；解析部分失败（视频截断等）返回
// 「已解析出的可用偏移 + err」，调用方按降级语义处理。
//
// **单一 req 模式（2026-10-04 用户指令：未上线，不做双接口无效兼容）**：统一
// KeyframeOffsets(KeyframeRequest)，Path+Reader 至少一个可用；实现方按自身能力选择
// （ffprobe 有 Path 走文件路径避免 stdin 大文件降级，go-mp4 内存解析 Reader 够用）。
type KeyframeIndexer interface {
	KeyframeOffsets(req KeyframeRequest) ([]int64, error)
}

// keyframeOffsetsDispatch 按实现能力分发：req.Path 非空且实现支持时用路径（ffprobe），
// 否则用 Reader。旧 KeyframeOffsets(r, size) 已移除（未上线无兼容需求）。
func keyframeOffsetsDispatch(idx KeyframeIndexer, req KeyframeRequest) ([]int64, error) {
	return idx.KeyframeOffsets(req)
}

// BlockletModeProvider 描述一种可用的 blocklet 分块模式（注册条目）。
// 注册后 secretdata 写路径按目标文件名类型（Kind）自动选型；全部未命中回落内置 fixed。
type BlockletModeProvider struct {
	// Mode 是 blocklet 模式名（写入 meta.BlockletMode），如 "video-keyframe"。
	Mode string
	// Kind 是适用文件类型（自动选型判据），如 "video/mp4"、"video"。
	Kind string
	// Manager 是管理器类型标识（选型判据/审计），如 "go-mp4"、"ffprobe"。
	Manager string
	// Indexer 是关键帧解析器；Mode 非 keyframe 时可为 nil。
	Indexer KeyframeIndexer
	// Fallback 是主解析失败时的备用解析器链（2026-10-04 方案 A：伪装扩展名/截断/
	// 异常容器时 go-mp4 失败 → 依次尝试 ffprobe 兜底）。装配层在 ffmpeg 可用时把
	// ffprobe 加为 go-mp4 的 fallback。按序尝试，首个成功者用其结果。
	Fallback []KeyframeIndexer
}

// ErrPlannerConflict 是 ResolveBlockletMode 的哨兵错误：同 Kind ≥2 个异名提供者
// （自动选型无法唯一命中，secretdata 绑定失败，不静默二择一）。
var ErrPlannerConflict = errors.New("shardseal: blocklet 模式冲突（同 Kind 多个异名提供者）")

// builtinFixed 是内置 fixed 提供者（未命中任何外部提供者时的默认兜底）。
var builtinFixed = BlockletModeProvider{Mode: "fixed"}

// blockletReg 是 blocklet 模式注册表（基于 pkg/plugin.Registry[T] 泛型框架）。
// builtin = builtinFixed——全部未命中时回落（FixedBlockletPlanner 语义）。
var blockletReg = plugin.New[BlockletModeProvider]("shardseal-blocklet", builtinFixed)

// RegisterBlockletMode 注册一个 blocklet 模式提供者（动态绑定）。priority 高者自动选型
// 优先（内置 fixed 为 0；外部提供者通常 >0）。同 Kind+Mode+Manager 同名注册 → 后注册
// 覆盖前注册（pkg/plugin 既有语义）。
func RegisterBlockletMode(p BlockletModeProvider, priority int) {
	blockletReg.Register(plugin.Plugin[BlockletModeProvider]{
		Name:     blockletModeName(p),
		Instance: p,
		Priority: priority,
	})
}

// UnregisterBlockletMode 移除一个 blocklet 模式提供者（动态解绑）。按 mode+kind 删除
// 全部同名组条目（含不同 Manager 的异名提供者）。已写卷不受影响（meta.BlockletMode 已
// 固化），读路径按 meta 决定，不依赖注册表。
func UnregisterBlockletMode(mode, kind string) {
	prefix := blockletModeKey(mode, kind) + "/"
	for _, name := range blockletReg.Names() {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			blockletReg.Delete(name)
		}
	}
}

// ResolveBlockletMode 按文件类型自动选型：收集 Kind 匹配的候选（**先精确、后通配前缀**）。
// 0 条 → 内置 fixed 兜底；1 条 → 命中；≥2 条不同提供者 → ErrPlannerConflict（绑定失败）。
//
// 通配语义：提供者可注册**容器族前缀** Kind（如 "video"），精确 Kind（如 "video/mp4"）
// 会匹配 "video" 前缀候选——ffprobe（覆盖全容器）注册 "video" 通配即可命中所有 video/*，
// go-mp4（仅 MP4）注册精确 "video/mp4"；装配按环境只注册一个生效解析器（ffprobe 存在
// 时注册通配 ffprobe、否则注册精确 go-mp4），保证行为一致（不双注册共存避免冲突）。
func ResolveBlockletMode(kind string) (BlockletModeProvider, error) {
	// 第一遍：精确 Kind 匹配。
	var matches []BlockletModeProvider
	for _, name := range blockletReg.Names() {
		p, ok := blockletReg.Get(name)
		if !ok {
			continue
		}
		if p.Kind == kind {
			matches = append(matches, p)
		}
	}
	// 第二遍（精确无命中）：容器族前缀通配（p.Kind 是 kind 的前缀，如 "video" ↔ "video/mp4"）。
	if len(matches) == 0 {
		for _, name := range blockletReg.Names() {
			p, ok := blockletReg.Get(name)
			if !ok {
				continue
			}
			if KindPrefixMatch(p.Kind, kind) {
				matches = append(matches, p)
			}
		}
	}
	switch len(matches) {
	case 0:
		return builtinFixed, nil
	case 1:
		return matches[0], nil
	default:
		return BlockletModeProvider{}, ErrPlannerConflict
	}
}

// KindPrefixMatch 判定 providerKind（容器族，如 "video"）是否匹配请求 kind（如
// "video/mp4"）：精确相等或请求 kind 以 providerKind+"/" 开头（测试与装配共用）。
func KindPrefixMatch(providerKind, requestKind string) bool {
	if providerKind == "" {
		return false // 空 Kind 不参与通配（避免误匹配一切）
	}
	return requestKind == providerKind || strings.HasPrefix(requestKind, providerKind+"/")
}

// blockletModeName 构造注册条目名（Name 唯一键：mode/kind/manager——同名同提供者
// 注册覆盖；不同 Manager 的异名提供者保留为独立条目，供冲突检测）。
func blockletModeName(p BlockletModeProvider) string {
	return blockletModeKey(p.Mode, p.Kind) + "/" + p.Manager
}

// blockletModeKey 是注册条目的组键（mode + kind；Unregister 按 mode+kind 精确匹配）。
func blockletModeKey(mode, kind string) string {
	return fmt.Sprintf("%s/%s", mode, kind)
}
