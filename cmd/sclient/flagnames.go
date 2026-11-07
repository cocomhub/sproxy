// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// 集中定义跨命令复用的旗标名字符串（S1192：注册与读取共享同一常量，防拼写漂移）。
const (
	flagOutputDir    = "output-dir"
	flagPollInterval = "poll-interval"
	flagURLFile      = "url-file"
	// flagToVolume 是**卷管理命令**（volume copy/move/rebalance）的目标卷 flag；
	// 文件命令（mv/cp 等）已改用**路径内卷 URI**（用户口径 2026-10-14），不再使用本 flag。
	flagToVolume      = "to-volume"
	flagFromVolume    = "from-volume"
	flagForce         = "force"
	flagVirtualSubnet = "virtual-subnet"
	flagAPIBase       = "api-base"
	flagReleaseBase   = "release-base"
	// cloud-download 三行为旗标（chain/submit 双命令复用；S1192 注册与读取共享常量）。
	flagTransferVolume    = "transfer-volume"
	flagTransferPath      = "transfer-path"
	flagDownloadLocal     = "download-local"
	flagSave              = "save"
	flagIntegrityMustPass = "integrity-must-pass"
	// flagMetaExtra 是转存产物的自定义元数据（JSON 或 @file；上传命令已有同名 flag）。
	flagMetaExtra = "meta-extra"
)
