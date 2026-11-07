// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pikpak 提供 PikPak 网盘中转后端（独立 module）。
//
// 设计（R1）：通用下载中转站的 PikPak 适配器。链路：
//
//	PikPak 分享 URL → 转存到个人网盘（share/restore）→ 官方 CLI 完整下载
//
// 关键事实（实测 2026-10-01）：
//   - PikPak 分享链接有官方大小限制（超过阈值只能拿 40-50% + 头尾），
//     中间段 Range 返回 416 —— 这是服务端反下载设计，非 bug；
//   - 转存到**个人网盘**后，官方直链（/drive/v1/files/{id}?usage=FETCH）
//     全 Range 可下（无中间空洞），可用官方 CLI 完整下载全长文件；
//   - 官方 CLI（pikpak.exe）走 OAuth device 授权（浏览器确认码），无密码/captcha 逆向；
//   - 免费账号限速 ~1.15MB/s（账号级，多线程不加速）；4.5GB 约 1 小时。
//
// 本包提供：
//   - Cli：官方 CLI 定位/自动安装（跨 darwin/linux/windows）与执行；
//   - Auth：OAuth device 登录（授权链接 + user_code + 轮询）；
//   - API：官方 REST（drive/v1）——分享详情/转存/列表/删除/直链；
//   - PikpakDownloader：实现 pkg/downloader.Downloader（注册表可插拔）；
//   - Storage：实现网盘卷存储（list/get/delete，可接 sproxy 卷体系）。
package pikpak
