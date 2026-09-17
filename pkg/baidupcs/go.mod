// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/baidupcs 是百度网盘存储后端的独立 Go module：
// 隔离 BaiduPCS-Go（Apache-2.0）裁剪库的依赖，避免污染 sproxy 主 module。
// 裁剪源：https://github.com/qjfoidnh/BaiduPCS-Go（Apache-2.0，commit 以 internal 注释标注）。

module github.com/cocomhub/sproxy/pkg/baidupcs

go 1.27

require github.com/cocomhub/sproxy v0.0.0

replace github.com/cocomhub/sproxy => ../..
