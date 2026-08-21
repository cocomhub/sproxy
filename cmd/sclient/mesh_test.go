// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestNewCmdMesh_Subcommands(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Use != "mesh" {
		t.Fatalf("expected Use 'mesh', got %q", cmd.Use)
	}
	subs := map[string]bool{"connect": false, "status": false}
	for _, c := range cmd.Commands() {
		if _, ok := subs[c.Name()]; ok {
			subs[c.Name()] = true
		}
	}
	for name, found := range subs {
		if !found {
			t.Errorf("missing subcommand: %s", name)
		}
	}
}

func TestNewCmdMeshConnect_ArgsAndFlags(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	connect := cmd.Commands()[0]
	if connect.Use != "connect <service> [-l :port]" {
		t.Fatalf("unexpected connect Use: %q", connect.Use)
	}
	for _, name := range []string{"listen", "webrtc", "hub", "token", "node-id"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("connect 缺少 flag: %s", name)
		}
	}
}

// TestDefaultMeshDial_FallsBackToRelay 验证选路：webrtc 打洞失败（目标无 p2p listen
// 或不可达）时回落 hub 中继。
func TestDefaultMeshDial_FallsBackToRelay(t *testing.T) {
	// 模拟 hub：/api/relay/stream 返回 502（中继也失败）→ 最终返回错误
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/relay/stream" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	target := &client.MeshService{Name: "svc", Node: "node-a", Addr: "127.0.0.1:22"}
	// signaler 指向不可达 hub（webrtc 打洞必然失败，验证回落）
	signaler := hub.NewHubSignaler("http://127.0.0.1:1", "", "local-node")

	_, err := defaultMeshDial(context.Background(), svc, signaler, target, "local-node")
	if err == nil {
		t.Fatal("expected error: webrtc 打洞失败且中继失败应报错")
	}
	// 错误应来自中继（webrtc 已回落），而非 webrtc 本身
	if !strings.Contains(err.Error(), "502") && !strings.Contains(err.Error(), "hub") {
		t.Fatalf("expected relay fallback error, got: %v", err)
	}
}

// TestMeshRelayPath_NoDialFrame 回归测试：relay 中继路径不得写 dial 帧。
// 实测发现的 bug：mesh connect 曾对 RelayStream 返回的流额外写 [4B len][{"dial":...}]
// 帧，导致数据流被污染（echo 返回帧头而非业务数据）。
// 修复后通过 meshDialFrameNeeded 精准判定：仅 webrtc 写帧，relay 不写。
func TestMeshDialFrameNeeded(t *testing.T) {
	tests := []struct {
		kind string
		want bool
	}{
		{"webrtc", true}, // 打洞直连对端，需写帧告知出口拨目标
		{"relay", false}, // hub 的 RelayStreamHandler 已写帧，客户端不得再写（实测 bug）
		{"", false},
		{"unknown", false},
	}
	for _, tc := range tests {
		if got := meshDialFrameNeeded(tc.kind); got != tc.want {
			t.Fatalf("meshDialFrameNeeded(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}
