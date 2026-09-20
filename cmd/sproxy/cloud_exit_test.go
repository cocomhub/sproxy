// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
)

func TestBuildCloudExitDial_MissingCreds_Fails(t *testing.T) {
	// sproxy:serial: 轻量构造验证，串行降低 cmd/sproxy 包并行度（SignalShutdown goroutine 上限敏感）
	cfg := server.Default()
	cfg.CloudDownloadExitNode = "node-exit"
	cfg.Mesh.HubURL = "https://hub.example.com:18083"
	// 缺 mesh.access_key/secret → fail-closed 报错。
	_, err := buildCloudExitDial(cfg)
	if err == nil || !strings.Contains(err.Error(), "mesh.access_key") {
		t.Fatalf("err = %v, want 含 mesh.access_key（fail-closed）", err)
	}
}

func TestBuildCloudExitDial_RemoteHub_WithCreds_OK(t *testing.T) {
	// sproxy:serial: 轻量构造验证，串行降低 cmd/sproxy 包并行度（SignalShutdown goroutine 上限敏感）
	cfg := server.Default()
	cfg.CloudDownloadExitNode = "node-exit"
	cfg.Mesh.HubURL = "https://hub.example.com:18083"
	cfg.Mesh.AccessKey = "ak"
	cfg.Mesh.AccessKeySecret = "sk"
	dial, err := buildCloudExitDial(cfg)
	if err != nil {
		t.Fatalf("buildCloudExitDial: %v", err)
	}
	if dial == nil {
		t.Fatalf("dial 不应为 nil")
	}
}

func TestBuildCloudExitDial_LocalHub_WithCreds_OK(t *testing.T) {
	// sproxy:serial: 轻量构造验证，串行降低 cmd/sproxy 包并行度（SignalShutdown goroutine 上限敏感）
	cfg := server.Default()
	cfg.CloudDownloadExitNode = "node-exit"
	cfg.Addr = ":18083" // 本机 hub（mesh.hub_url 空）回落本机 HTTP 面派生
	cfg.Mesh.AccessKey = "ak"
	cfg.Mesh.AccessKeySecret = "sk"
	cfg.TLS.Enabled = true
	dial, err := buildCloudExitDial(cfg)
	if err != nil {
		t.Fatalf("buildCloudExitDial: %v", err)
	}
	if dial == nil {
		t.Fatalf("dial 不应为 nil")
	}
}
