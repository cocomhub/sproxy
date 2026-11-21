// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestParseBatchPlan 锁定清单解析（TSV，允许省略列/注释/空行）。
func TestParseBatchPlan(t *testing.T) {
	t.Parallel()
	in := "# 注释\n\nhttps://a/1\tjav/x/1.mp4\tabc123\nhttps://a/2\nhttps://a/3\tjav/x/3.mp4\n"
	got, err := parseBatchPlan(bufio.NewScanner(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应解析 3 条，got %d", len(got))
	}
	if got[0].URL != "https://a/1" || got[0].Path != "jav/x/1.mp4" || got[0].Expect != "abc123" {
		t.Fatalf("首条解析错误: %+v", got[0])
	}
	if got[1].Path != "" || got[1].Expect != "" {
		t.Fatalf("省略列应留空: %+v", got[1])
	}
}

// TestNewCmdCloudDownloadBatch_Flags 锁定 batch 命令参数。
func TestNewCmdCloudDownloadBatch_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdCloudDownloadBatch(nil, cli.IOStreams{})
	for _, name := range []string{"file", "concurrency", "wait", "timeout", "interval"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("缺少 --%s flag", name)
		}
	}
}
