// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"errors"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

// KeyframeIndexer 是视频关键帧解析接口（可插拔，由 pkg/cryptox/ext/keyframe 等子模块
// 实现）。返回关键帧在原始文件中的**绝对字节偏移**（升序）；解析部分失败（视频截断等）
// 返回「已解析出的可用偏移 + err」，调用方按降级语义处理。
type KeyframeIndexer interface {
	KeyframeOffsets(r io.ReaderAt, fileSize int64) ([]int64, error)
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

// ResolveBlockletMode 按文件类型自动选型：收集 Kind 匹配的候选；0 条 → 内置 fixed 兜底；
// 1 条 → 命中；≥2 条不同提供者 → ErrPlannerConflict（绑定失败）。
func ResolveBlockletMode(kind string) (BlockletModeProvider, error) {
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
	switch len(matches) {
	case 0:
		return builtinFixed, nil
	case 1:
		return matches[0], nil
	default:
		return BlockletModeProvider{}, ErrPlannerConflict
	}
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
