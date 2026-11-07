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
	_ = cmd.Flags().Set(flagFromVolume, "volA")
	_ = cmd.Flags().Set(flagToVolume, "volB")
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
	_ = cmd.Flags().Set(flagFromVolume, "volA")
	_ = cmd.Flags().Set(flagToVolume, "volB")
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
	_ = cmd.Flags().Set(flagFromVolume, "volA")
	_ = cmd.Flags().Set(flagToVolume, "volB")
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

// TestVolumeOps_RefreshCmdAllVolumes 钉住「无参 = 全部卷」：请求不带 volume 查询参数
// （服务端据此清空全部卷状态缓存）。默认缓存永不过期后，这是感知进程外改动的主要入口。
func TestVolumeOps_RefreshCmdAllVolumes(t *testing.T) {
	t.Parallel()
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes/refresh" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "全部卷状态缓存已清空"})
	}))
	t.Cleanup(ts.Close)

	var buf strings.Builder
	cmd := newCmdVolumeRefresh(clientfactory.NewMock(newTestClient(t, ts.URL), nil),
		cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume refresh（无参）失败: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("无参 refresh 不应带 volume 参数，got %q", gotQuery)
	}
	if !strings.Contains(buf.String(), "全部卷") {
		t.Fatalf("输出应说明清空了全部卷，got %s", buf.String())
	}
}

// TestVolumeOps_RefreshCmdSingleVolume 带卷名 ⇒ 只清该卷。
func TestVolumeOps_RefreshCmdSingleVolume(t *testing.T) {
	t.Parallel()
	var gotVol string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVol = r.URL.Query().Get("volume")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
	}))
	t.Cleanup(ts.Close)

	cmd := newCmdVolumeRefresh(clientfactory.NewMock(newTestClient(t, ts.URL), nil),
		cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"vault:/"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume refresh vault 失败: %v", err)
	}
	if gotVol != "vault" {
		t.Fatalf("应清空 vault（卷 URI 需归一），got %q", gotVol)
	}
}
