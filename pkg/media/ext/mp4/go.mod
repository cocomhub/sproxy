// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/media/ext/mp4 是 MP4/MOV 视频关键帧解析的可插拔扩展独立 module（仿 xfer/ext 模式）。
// 第三方库（go-mp4）隔离在独立 module——主仓 go.mod 不直接依赖，避免第三方依赖污染
// 主仓依赖树；本 module 实现 shardseal.KeyframeIndexer 接口，经 replace 指令接入主仓。

module github.com/cocomhub/sproxy/pkg/media/ext/mp4

go 1.27

require (
	github.com/abema/go-mp4 v1.7.3
	github.com/cocomhub/sproxy v0.0.0
)

require github.com/google/uuid v1.1.2 // indirect

replace github.com/cocomhub/sproxy => ../../../..

