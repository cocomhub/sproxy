// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdCloudDownloadRetry_Flags 锁定重试命令的选择器参数（2026-11-21）。
func TestNewCmdCloudDownloadRetry_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdCloudDownloadRetry(nil, cli.IOStreams{})
	for _, name := range []string{"task-id", "url", "transfer-path", "group-id", "force", "limit"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("缺少 --%s flag", name)
		}
	}
	if cmd.Flags().Lookup("include-completed-damaged").DefValue != "true" {
		t.Fatal("--include-completed-damaged 默认应为 true（damaged 重下自愈）")
	}
}
