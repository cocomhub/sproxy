// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/integrity/ext/video 是视频语义校验器的独立 Go module（插件模式，对齐
// pkg/volume/ext/baidupcs 仓库内 module replace 模式）。
//
// 定位：视频校验复用 pkg/media/ffprobe（ffprobe 子进程解析容器关键帧偏移），
// 从主 module 的 pkg/integrity 校验器族中隔离——ffprobe 是唯一需外部依赖
// （ffmpeg 生态）的校验器，独立 module 使主 module go.mod 不引入外部依赖。
//
// 方案（对齐 spec §5 插件扩展）：本 module 只含自有薄 VideoChecker（实现
// pkg/integrity.Checker 接口），经 init() 注册进 pakage 级默认注册表，cmd/sproxy
// 空白导入装配。

module github.com/cocomhub/sproxy/pkg/integrity/ext/video

go 1.27

require github.com/cocomhub/sproxy v0.0.0

require golang.org/x/crypto v0.57.0 // indirect

replace github.com/cocomhub/sproxy => ../../../..
