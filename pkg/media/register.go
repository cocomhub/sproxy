// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package media 提供媒体解析能力的**共享装配**（2026-10-14）：视频关键帧分块
// （video-keyframe）的 blocklet 模式提供者注册。sproxy（cmd/sproxy）与 sclient
// `sealfile` 本地加密共用同一份注册逻辑——两端能力自动对齐，未来新增解析器/容器族
// （图片打包等）只改本文件一处，两端同时获得。
package media

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

// RegisterKeyframeBackends 注册 video-keyframe 提供者（幂等）。装配层（sproxy 启动 /
// sclient sealfile 首次加密）调用。本函数只做 Once 包装（测试直接测纯函数，
// 避免共享 Once 状态）。
//
// **启动可观测性（评审 I-4 修复）**：打 info 日志标明注册的解析器组合——运维无需翻
// 代码/配置即可得知能力边界（MP4 走 go-mp4、其它容器是否有 ffprobe 覆盖）。
func RegisterKeyframeBackends() {
	registerKeyframeOnce.Do(func() {
		has := FFprobeAvailable()
		// 双注册共存：go-mp4 精确 + （有 ffprobe 时）ffprobe 通配。
		// go-mp4 提供者的 fallback 由装配层显式注入（`has` 决定是否可用 ffprobe 兜底——
		// **用户裁定 2026-10-04**：应该判断存在 ffprobe 才能用 ffprobe；而非在
		// KeyframeProviderFor(false) 内部二次探测（那里 hasFFprobe 恒 false 导致死代码，
		// 四轮评审独立命中 Critical C1）。
		gp := KeyframeProviderFor(false)
		if has {
			gp.Fallback = []shardseal.KeyframeIndexer{ffprobe.Indexer{}}
		}
		RegisterKeyframeProvider(gp)
		if has {
			RegisterKeyframeProvider(KeyframeProviderFor(true))
		}
		if has {
			slog.Info("keyframe: MP4/MOV 走 go-mp4（纯 Go 可靠、内存可控）；MKV/TS/AVI 走 ffprobe（全格式覆盖，fMP4 正常解析）")
		} else {
			slog.Info("keyframe: 未检测到 ffprobe——MP4/MOV 走 go-mp4；MKV/TS/AVI 走默认 fixed（渐进增强，可装 ffmpeg 启用全格式）")
		}
	})
}

// SetKeyframeMetricHooks 注入 fMP4 统计 hook（两个解析器都注入；fn 为
// server.Metrics.RecordKeyframeFragmented，nil 安全）。包级 hook 经 atomic setter 设置，
// 运行期 atomic 读触发——无数据竞争（与 usageRecorder 模式对齐但显式内存屏障）。
func SetKeyframeMetricHooks(fn func()) {
	mp4.SetOnFragmentedMP4(fn)
	ffprobe.SetOnFragmentedMP4(fn)
}

// RegisterKeyframeProvider 注册单个解析器提供者（装配与测试共用）。
func RegisterKeyframeProvider(p shardseal.BlockletModeProvider) {
	shardseal.RegisterBlockletMode(p, 1)
}

// FFprobeAvailable 检测 PATH 中是否存在 ffprobe（渐进增强判定）。
func FFprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// KeyframeProviderFor 构造解析器提供者（纯函数，可测）：
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
func KeyframeProviderFor(hasFFprobe bool) shardseal.BlockletModeProvider {
	if hasFFprobe {
		return shardseal.BlockletModeProvider{
			Mode:    shardseal.BlockletModeVideoKeyframe,
			Kind:    "video",
			Manager: "ffprobe",
			Indexer: ffprobe.Indexer{},
		}
	}
	gp := shardseal.BlockletModeProvider{
		Mode:    shardseal.BlockletModeVideoKeyframe,
		Kind:    "video/mp4",
		Manager: "go-mp4",
		Indexer: mp4.Indexer{},
	}
	// **Fallback 由装配层注入（评审 C1 修复 + 用户裁定 2026-10-04）**：KeyframeProviderFor
	// 是纯函数，fallback 是否可用 ffprobe 由装配层（RegisterKeyframeBackends 的 `has`）
	// 决定——函数内不二次探测（否则 false 分支恒不可达 = 死代码，四轮评审命中）。
	return gp
}
