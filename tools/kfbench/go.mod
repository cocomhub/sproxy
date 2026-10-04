// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// tools/kfbench 是独立 module（仿 cmd/sproxy 模式）：关键帧解析引擎对比工具。
// 独立 go.mod 避免把 go-mp4 第三方依赖带进根 go.mod（根 go.mod 只保留 replace，
// 不 require mp4——「主仓零第三方」纪律；工具运行时经 go.work / replace 联动根与 mp4）。

module github.com/cocomhub/sproxy/tools/kfbench

go 1.27

require github.com/cocomhub/sproxy/pkg/media/ext/mp4 v0.0.0

require (
	github.com/abema/go-mp4 v1.7.3 // indirect
	github.com/cocomhub/sproxy v0.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
)

replace github.com/cocomhub/sproxy => ../..

replace github.com/cocomhub/sproxy/pkg/media/ext/mp4 => ../../pkg/media/ext/mp4
