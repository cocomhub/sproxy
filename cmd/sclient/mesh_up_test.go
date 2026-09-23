// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_up_test.go 验证 mesh up 命令注册（roadmap P2 VPN 模式最小集）：
//  1. mesh 命令包含 up 子命令。
//  2. up --help 输出虚拟子网说明。

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// TestMeshUp_Registered mesh 命令含 up 子命令。
func TestMeshUp_Registered(t *testing.T) {
	t.Parallel()
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	root := &cobra.Command{Use: "test"}
	mesh := NewCmdMesh(clientfactory.NewMock(nil, nil), ios, nil)
	root.AddCommand(mesh)
	up := findSub(mesh, "up")
	if up == nil {
		t.Fatal("mesh 应含 up 子命令")
	}
	if !strings.Contains(up.Short, "虚拟子网") {
		t.Fatalf("up.Short = %q", up.Short)
	}
}

func findSub(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
