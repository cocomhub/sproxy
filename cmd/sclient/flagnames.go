// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// 集中定义跨命令复用的旗标名字符串（S1192：注册与读取共享同一常量，防拼写漂移）。
const (
	flagOutputDir     = "output-dir"
	flagPollInterval  = "poll-interval"
	flagURLFile       = "url-file"
	flagFromVolume    = "from-volume"
	flagToVolume      = "to-volume"
	flagVirtualSubnet = "virtual-subnet"
	flagAPIBase       = "api-base"
	flagReleaseBase   = "release-base"
	// cloud-download 三行为旗标（chain/submit 双命令复用；S1192 注册与读取共享常量）。
	flagTransferVolume = "transfer-volume"
	flagTransferPath   = "transfer-path"
	flagDownloadLocal  = "download-local"
	flagSave           = "save"
	flagForceIntegrity = "force-integrity"
)
