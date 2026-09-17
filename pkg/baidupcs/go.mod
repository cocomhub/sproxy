// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/baidupcs 是百度网盘（BaiduPCS）存储后端的独立 Go module。
//
// 方案（R2）：**不 fork 裁剪核心库进本仓库**——直接引用外部 fork
// github.com/cocomhub/BaiduPCS-Go（fork 自 qjfoidnh/BaiduPCS-Go，module 声明保持
// qjfoidnh/BaiduPCS-Go 一行不改 → Sync fork 零冲突），经 replace 指令接入。
// 本 module 只含自有薄 adapter（Adapter 接口 / Storage 接口 / plugin 注册），
// 避免开源实现受 sproxy addlicense/lint 等强校验污染。

module github.com/cocomhub/sproxy/pkg/baidupcs

go 1.27

replace github.com/qjfoidnh/BaiduPCS-Go => github.com/cocomhub/BaiduPCS-Go v0.0.0-20260909034501-1b9131817aaf

replace github.com/cocomhub/sproxy => ../..

require (
	github.com/cocomhub/sproxy v0.0.0
	github.com/qjfoidnh/BaiduPCS-Go v0.0.0-20260909034501-1b9131817aaf
)

require (
	github.com/bitly/go-simplejson v0.5.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/kardianos/osext v0.0.0-20190222173326-2bc1f35cddc0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.9 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/olekukonko/tablewriter v0.0.4 // indirect
	github.com/qjfoidnh/Baidu-Login v1.4.1 // indirect
	github.com/qjfoidnh/baidu-tools v1.2.0 // indirect
	github.com/rs/dnscache v0.0.0-20230804202142-fc85eb664529 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
)
