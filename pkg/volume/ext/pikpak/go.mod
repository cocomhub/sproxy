// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// pkg/volume/ext/pikpak 是 PikPak 网盘中转后端的独立 Go module。
//
// 定位：通用下载中转站（P6① 可复用扩展工具集合）的 PikPak 适配器——
// 把「PikPak 分享 → 转存个人网盘 → 官方 CLI 下载」链路封装成：
//   - StorageAPI：网盘卷（list/put/get/delete/copy），可接 sproxy 同步/卷体系；
//   - downloader.Downloader：分享 URL → 转存 + CLI 下载，可接 pkg/downloader 注册表；
//   - CLI 安装/查找（跨 darwin/linux/windows）与 OAuth device 登录。
//
// 方案（R1）：**不引入 PikPak 逆向 API SDK**——全部经官方 CLI（pikpak.exe，
// OAuth 授权，无密码/captcha 逆向）与官方 REST（drive/v1 系列）。CLI 走官方
// Connected Apps 通道，个人网盘文件完整可下（分享链接 40-50% 限制与账号级
// 限速是服务端行为，本模块只负责编排与状态流转）。
//
// 模块内只含自有实现（无外部大依赖），避免开源逆向实现受 sproxy 强校验污染。

module github.com/cocomhub/sproxy/pkg/volume/ext/pikpak

go 1.27

replace github.com/cocomhub/sproxy => ../../../..

require github.com/cocomhub/sproxy v0.0.0
