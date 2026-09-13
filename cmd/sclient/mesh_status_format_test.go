// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_status_format_test.go 钉住 `mesh status --server` 的输出格式化（W4）：
// 与 Web UI 状态卡同口径 —— 面（地址 + pin 数）、节点角色（**未运行显式标注**）、hub/信令；
// 未启用的面/角色**不输出**（不产生无意义空行）。

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMeshServerStatusLines(t *testing.T) {
	full := &client.MeshStatus{
		RemoteRead:  &client.MeshFaceStatus{Enabled: true, Addr: "127.0.0.1:19000", Pinned: 2},
		RemoteWrite: &client.MeshFaceStatus{Enabled: true, Addr: "127.0.0.1:19001", Pinned: 1},
		Node:        &client.MeshNodeStatus{Running: true, NodeID: "node-b", WebRTC: true, Services: []string{"volread", "volwrite"}},
		HubURL:      "https://hub.example.com:18083", SignalingEnabled: true,
	}
	lines := strings.Join(meshServerStatusLines(full), "\n")
	for _, want := range []string{"只读面", "127.0.0.1:19000", "pin=2", "写面", "127.0.0.1:19001",
		"node-b", "运行中", "webrtc=true", "volread/volwrite", "https://hub.example.com:18083", "已启用"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, lines)
		}
	}

	// 配置启用但角色未运行：必须显式标注「未运行」（最需要被看见的状态）。
	notRunning := &client.MeshStatus{
		RemoteRead: &client.MeshFaceStatus{Enabled: true, Addr: "127.0.0.1:19000", Pinned: 0},
		Node:       &client.MeshNodeStatus{Running: false, NodeID: "node-b"},
	}
	lines2 := strings.Join(meshServerStatusLines(notRunning), "\n")
	if !strings.Contains(lines2, "未运行") {
		t.Fatalf("未运行必须显式标注:\n%s", lines2)
	}
	if strings.Contains(lines2, "写面") {
		t.Fatalf("未启用的写面不应输出:\n%s", lines2)
	}
	if !strings.Contains(lines2, "hub: 本机") || !strings.Contains(lines2, "信令: 未启用") {
		t.Fatalf("未配 hub/信令时应显示「本机/未启用」:\n%s", lines2)
	}

	// 空结构（全关）只输出标题 + hub/信令行，不出现任何面/角色行。
	empty := strings.Join(meshServerStatusLines(&client.MeshStatus{}), "\n")
	for _, banned := range []string{"只读面", "写面", "节点角色"} {
		if strings.Contains(empty, banned) {
			t.Fatalf("全关时不应输出 %q:\n%s", banned, empty)
		}
	}

	// nil 输入不 panic。
	if got := meshServerStatusLines(nil); len(got) == 0 {
		t.Fatal("nil 输入应给出提示行")
	}
}
