// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// keyframe_register.go 是视频关键帧分块的装配点：注册 shardseal 的 blocklet 模式提供者
// （video-keyframe）。解析器**双注册共存**（真实 4.6GB 文件实测决策，2026-10-04）：
//
//   - go-mp4（pkg/media/ext/mp4，纯 Go 零依赖）→ 精确 Kind="video/mp4"：MP4/MOV 走它。
//     实测：4.6GB 真实 MP4 完整解析 24958 关键帧、内存 17MB（修复 mdat Expand bug 后）、
//     不依赖解码器（对损伤文件比 ffprobe 可靠——ffprobe 因 h264 解码中断只解析前 14293 帧）。
//   - ffprobe（pkg/media/ffprobe）→ 通配 Kind="video"：MKV/WebM/TS/AVI 等非 MP4 容器走它
//     （go-mp4 只认 ISO-BMFF，无法处理 EBML/MPEG-TS/RIFF）。
//
// `ResolveBlockletMode` 先精确后通配（planner_registry.go）→ MP4 命中 go-mp4（轻、可靠、
// 内存可控），其它容器命中 ffprobe（全格式覆盖）。ffprobe 存在时两者共存注册；无 ffprobe
// 时仅 go-mp4（MP4 优化、其它容器默认 fixed——渐进增强不硬依赖）。
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
// 调用。本函数只做 Once 包装（测试直接测纯函数，避免共享 Once 状态）。
//
// **启动可观测性（评审 I-4 修复）**：打 info 日志标明注册的解析器组合——运维无需翻
// 代码/配置即可得知能力边界（MP4 走 go-mp4、其它容器是否有 ffprobe 覆盖）。
func registerKeyframeBackend() {
	registerKeyframeOnce.Do(func() {
		has := ffprobeAvailable()
		// 双注册共存：go-mp4 精确 + （有 ffprobe 时）ffprobe 通配。
		registerKeyframeProvider(keyframeProviderFor(false))
		if has {
			registerKeyframeProvider(keyframeProviderFor(true))
		}
		if has {
			slog.Info("keyframe: MP4/MOV 走 go-mp4（纯 Go 可靠、内存可控）；MKV/TS/AVI 走 ffprobe（全格式覆盖，fMP4 正常解析）")
		} else {
			slog.Info("keyframe: 未检测到 ffprobe——MP4/MOV 走 go-mp4；MKV/TS/AVI 走默认 fixed（渐进增强，可装 ffmpeg 启用全格式）")
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

// keyframeProviderFor 构造解析器提供者（纯函数，可测）：
//   - hasFFprobe=true → 通配 Kind="video" 的 ffprobe（非 MP4 容器兜底）；
//   - hasFFprobe=false → 精确 Kind="video/mp4" 的 go-mp4（MP4 主路径，零依赖可靠）。
//
// 装配对两者**都**注册（共存）：go-mp4 精确优先（MP4 轻、可靠、内存可控，实测 4.6GB
// 完整解析），ffprobe 通配覆盖其它容器。真实文件实测证明两引擎在 MP4 上结果一致
// （干净视频 10/10 帧相同；4.6GB 上 ffprobe 中断点前 14293 帧与 go-mp4 完全一致）。
//
// **Fallback 接线（评审 I-1 修复，2026-10-04）**：有 ffprobe 时，go-mp4 提供者携带
// Fallback=[ffprobe]——伪装扩展名/截断/异常容器（如 TS 流改名 .mp4）主解析失败时
// planner 依次尝试 ffprobe 兜底（方案 A 真正生效，非死代码）。
func keyframeProviderFor(hasFFprobe bool) shardseal.BlockletModeProvider {
	if hasFFprobe {
		return shardseal.BlockletModeProvider{
			Mode:    "video-keyframe",
			Kind:    "video",
			Manager: "ffprobe",
			Indexer: ffprobe.Indexer{},
		}
	}
	gp := shardseal.BlockletModeProvider{
		Mode:    "video-keyframe",
		Kind:    "video/mp4",
		Manager: "go-mp4",
		Indexer: mp4.Indexer{},
	}
	// go-mp4 精确提供者带 ffprobe fallback（有 ffmpeg 环境时兜底伪装/截断容器）。
	if hasFFprobe {
		gp.Fallback = []shardseal.KeyframeIndexer{ffprobe.Indexer{}}
	}
	return gp
}
