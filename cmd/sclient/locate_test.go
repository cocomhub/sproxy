// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdLocate_HasVolumeFlag 锁定：`locate` 必须支持 `--volume`。
//
// 为什么（2026-11-17 实测）：反查请求的 `volume` 由 client 的卷上下文决定，而 SproxySig
// 直连（无 `cd` 卷上下文）时恒为空 ⇒ 服务端返回 400「缺少 volume 或 checksum」⇒
// 该命令在自动化场景（sdserver 等）**不可用** ✗。故必须能显式指定卷。
func TestNewCmdLocate_HasVolumeFlag(t *testing.T) {
	t.Parallel()
	cmd := NewCmdLocate(clientfactory.Factory(nil), cli.IOStreams{})
	if cmd.Flags().Lookup("volume") == nil {
		t.Fatal("locate 命令必须提供 --volume（否则非 cd 上下文下恒 400）")
	}
}
