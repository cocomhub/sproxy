// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
)

// newVolumeOpsCmdMock 返回 mock 服务端（copy/move/rebalance 端点）。
func newVolumeOpsCmdMock(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/volumes/copy", "/api/volumes/move":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
		case "/api/volumes/rebalance":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "rebalanced", "moved": 3, "bytes_moved": 99, "remaining": 7})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestVolumeOps_CopyCmd(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsCmdMock(t)
	svc := newTestClient(t, ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdVolumeCopy(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{CurrentDir: "dir"})
	cmd.SetArgs([]string{"f.txt"})
	_ = cmd.Flags().Set("from-volume", "volA")
	_ = cmd.Flags().Set("to-volume", "volB")
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume copy failed: %v", err)
	}
	if !strings.Contains(buf.String(), "复制完成") {
		t.Errorf("copy output 缺成功文案：%s", buf.String())
	}
}

func TestVolumeOps_CopyCmdMissingTo(t *testing.T) {
	t.Parallel()
	svc := newTestClient(t, newVolumeOpsCmdMock(t).URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := newCmdVolumeCopy(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"f.txt"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("volume copy 缺 --to-volume 应报错，got nil")
	}
}

func TestVolumeOps_MoveCmd(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsCmdMock(t)
	svc := newTestClient(t, ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdVolumeMove(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"f.txt"})
	_ = cmd.Flags().Set("from-volume", "volA")
	_ = cmd.Flags().Set("to-volume", "volB")
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume move failed: %v", err)
	}
	if !strings.Contains(buf.String(), "移动完成") {
		t.Errorf("move output 缺成功文案：%s", buf.String())
	}
}

func TestVolumeOps_RebalanceCmd(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsCmdMock(t)
	svc := newTestClient(t, ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdVolumeRebalance(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	_ = cmd.Flags().Set("from-volume", "volA")
	_ = cmd.Flags().Set("to-volume", "volB")
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume rebalance failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已提交") || !strings.Contains(buf.String(), "moved=3") {
		t.Errorf("rebalance output 缺提交/统计：%s", buf.String())
	}
}

func TestVolumeOps_RebalanceCmdMissingArgs(t *testing.T) {
	t.Parallel()
	svc := newTestClient(t, newVolumeOpsCmdMock(t).URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := newCmdVolumeRebalance(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if err := cmd.Execute(); err == nil {
		t.Fatal("volume rebalance 缺 from/to 应报错，got nil")
	}
}
