// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_acl_format_test.go 钉住 `mesh acl` 的输出格式化（W3）：本 owner 的跨节点授权列表。
// 指纹**不截断**（要能直接与配置文件里的值逐字对照），但列对齐便于一眼看出「谁能写」。

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMeshACLLines(t *testing.T) {
	acl := &client.MeshACL{
		Owner: "alice",
		Entries: []client.MeshACLEntry{
			{Volume: "main", Node: "node-a", Fingerprint: "sha256:" + strings.Repeat("a", 64), Scope: "rw"},
			{Volume: "share", Node: "node-c", Fingerprint: "sha256:" + strings.Repeat("b", 64), Scope: "read"},
		},
	}
	lines := strings.Join(meshACLLines(acl), "\n")
	for _, want := range []string{"alice", "main", "node-a", "rw", "sha256:" + strings.Repeat("a", 64),
		"share", "node-c", "read"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, lines)
		}
	}
	// 指纹必须在：截断会让用户无法与配置逐字对照。
	if strings.Contains(lines, "…") {
		t.Fatalf("指纹不应截断:\n%s", lines)
	}
}

// TestMeshACLLines_Empty 钉住「无授权」是正常态（提示而非空输出），且不 panic。
func TestMeshACLLines_Empty(t *testing.T) {
	lines := meshACLLines(&client.MeshACL{Owner: "alice"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "alice") || !strings.Contains(joined, "无") {
		t.Fatalf("空授权应给出提示并带上 owner:\n%s", joined)
	}
	if got := meshACLLines(nil); len(got) == 0 {
		t.Fatal("nil 输入应给出提示行")
	}
}
