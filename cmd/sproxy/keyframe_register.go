// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// keyframe_register.go 是视频关键帧分块的装配点：把 pkg/cryptox/ext/keyframe（go-mp4
// 解析器子模块）注册为 shardseal 的 blocklet 模式提供者（video-keyframe / Kind=video）。
// 主 module 唯一 import 子模块的点——shardseal/secretdata 保持 MP4-free，仅消费接口。
//
// 装配语义：pkg/plugin.Registry 同名注册覆盖；sync.Once 防重复装配。注册后 secretdata
// 写路径按视频扩展名自动选型 video-keyframe（未注册回落 fixed）。

import (
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/ext/keyframe"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// registerKeyframeOnce 防止重复装配（多装配/多测试并发调 runServer 时只注册一次）。
var registerKeyframeOnce sync.Once

// registerKeyframeBackend 注册 video-keyframe 提供者（幂等）。装配层在 server 启动时
// 调用；测试可直接调用验证注册语义。
func registerKeyframeBackend() {
	registerKeyframeOnce.Do(func() {
		shardseal.RegisterBlockletMode(shardseal.BlockletModeProvider{
			Mode:    "video-keyframe",
			Kind:    "video",
			Manager: "go-mp4",
			Indexer: keyframe.Indexer{},
		}, 1)
	})
}
