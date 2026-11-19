// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdVolumeOrphans_Flags 锁定 `volume orphans` 的参数（用户 2026-11-20：异步轮询）。
func TestNewCmdVolumeOrphans_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVolumeOrphans(nil, cli.IOStreams{})
	for _, name := range []string{"volume", "refresh", "wait", "interval"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("缺少 --%s flag", name)
		}
	}
	if cmd.Flags().Lookup("wait").DefValue != "true" {
		t.Fatalf("--wait 默认应为 true（轮询到完成）")
	}
}
