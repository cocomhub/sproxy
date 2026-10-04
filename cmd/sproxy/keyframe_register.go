// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// keyframe_register.go 是视频关键帧分块的装配点：注册 shardseal 的 blocklet 模式提供者
// （video-keyframe），解析器按运行环境二选一：
//
//   - ffprobe 存在（PATH 可找到）→ 注册**通配 Kind="video"** 的 ffprobe 提供者（覆盖全部
//     容器：MP4/MOV/MKV/WebM/TS/AVI…），与 go-mp4 相比功能覆盖、正确性（ffmpeg 生态）、
//     可维护性（单二进制替代 N 个纯 Go 库）全面胜出；
//   - ffprobe 不存在 → 注册精确 Kind="video/mp4" 的 go-mp4 提供者（零依赖兜底，仅 MP4/MOV）。
//
// 装配保证**行为一致**：两种解析器不共存注册（同一时间只有一个生效）——ffprobe 存在时
// 全走 ffprobe（含 MP4），避免「MP4 用 go-mp4、其它用 ffprobe」的不一致；无 ffmpeg 环境
// 回落 go-mp4（MP4）或默认 fixed（其它格式，渐进增强不硬依赖）。
//
// 主 module 唯一 import 子模块的点——shardseal/secretdata 保持解析器无关，仅消费
// KeyframeIndexer 接口。

import (
	"log/slog"
	"os/exec"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	mp4 "github.com/cocomhub/sproxy/pkg/media/ext/mp4"
	"github.com/cocomhub/sproxy/pkg/media/ffprobe"
)

// registerKeyframeOnce 防止重复装配（多装配/多测试并发调 runServer 时只注册一次）。
var registerKeyframeOnce sync.Once

// registerKeyframeBackend 注册 video-keyframe 提供者（幂等）。装配层在 server 启动时
// 调用。选型逻辑（ffprobe 可用 → 通配 ffprobe；否则 go-mp4）抽到 keyframeProviderFor，
// 本函数只做 Once 包装（测试直接测纯函数，避免共享 Once 状态）。
//
// **启动可观测性（评审 I-4 修复）**：打一条 info 日志标明当前生效解析器——运维无需翻
// 代码/配置即可得知「无 ffmpeg 环境回落 go-mp4（MP4 优化、其它 fixed）」还是「ffprobe
// 覆盖全部容器」。此前无日志，静默选型导致运维误判能力边界。
func registerKeyframeBackend() {
	registerKeyframeOnce.Do(func() {
		has := ffprobeAvailable()
		p := keyframeProviderFor(has)
		registerKeyframeProvider(p)
		if has {
			slog.Info("keyframe: 视频关键帧解析器=ffprobe（通配 video，覆盖全部容器；fMP4 正常解析）")
		} else {
			slog.Info("keyframe: 未检测到 ffprobe，回落 go-mp4（仅 MP4/MOV；MKV/TS/AVI 走默认 fixed）")
		}
	})
}

// registerKeyframeMetricHooks 注入 fMP4 统计 hook（两个解析器都注入；fn 为
// server.Metrics.RecordKeyframeFragmented，nil 安全）。包级 hook 经 atomic setter 设置，
// 运行期 atomic 读触发——无数据竞争（与 usageRecorder 模式对齐但显式内存屏障）。
func registerKeyframeMetricHooks(fn func()) {
	mp4.SetOnFragmentedMP4(fn)
	ffprobe.SetOnFragmentedMP4(fn)
}

// registerKeyframeProvider 注册单个解析器提供者（装配与测试共用）。
func registerKeyframeProvider(p shardseal.BlockletModeProvider) {
	shardseal.RegisterBlockletMode(p, 1)
}

// ffprobeAvailable 检测 PATH 中是否存在 ffprobe（渐进增强判定）。
func ffprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// keyframeProviderFor 按环境选型解析器提供者（纯函数，可测）：
//   - ffprobe 可用 → 通配 Kind="video" 的 ffprobe（覆盖全部容器族，含 MP4——行为一致）；
//   - 否则 → 精确 Kind="video/mp4" 的 go-mp4（零依赖兜底，仅 MP4/MOV）。
func keyframeProviderFor(hasFFprobe bool) shardseal.BlockletModeProvider {
	if hasFFprobe {
		return shardseal.BlockletModeProvider{
			Mode:    "video-keyframe",
			Kind:    "video",
			Manager: "ffprobe",
			Indexer: ffprobe.Indexer{},
		}
	}
	return shardseal.BlockletModeProvider{
		Mode:    "video-keyframe",
		Kind:    "video/mp4",
		Manager: "go-mp4",
		Indexer: mp4.Indexer{},
	}
}
