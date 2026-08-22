// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestRelayCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelay(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	if cmd.Use != "relay" {
		t.Errorf("expected Use=relay, got %s", cmd.Use)
	}
	if cmd.Short != "中继节点管理" {
		t.Errorf("expected Short=中继节点管理, got %s", cmd.Short)
	}
}

func TestRelayCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelay(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	for _, name := range []string{"start", "status", "stop", "remove-node", "stats"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestRelayStartCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard})
	if cmd.Use != "start" {
		t.Errorf("expected Use 'start', got %q", cmd.Use)
	}
	if cmd.Short != "启动中继节点，连接到 Hub" {
		t.Errorf("expected Short '启动中继节点，连接到 Hub', got %q", cmd.Short)
	}
	for _, name := range []string{"hub", "local", "node-id", "token", "dial-allow", "service", "dial-allow-cidr"} {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("missing flag: %s", name)
		}
	}
}

func TestRelayStopCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStop(cli.IOStreams{Out: io.Discard})
	if cmd.Use != "stop" {
		t.Errorf("expected Use 'stop', got %q", cmd.Use)
	}
	if cmd.Short != "停止中继节点" {
		t.Errorf("expected Short '停止中继节点', got %q", cmd.Short)
	}
}

func TestRelayStatsCmd_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/stats" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"node_count":3}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStats(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stats command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "3") {
		t.Errorf("expected output to contain node count, got: %s", buf.String())
	}
}

func TestRelayStatusCmd_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"node-1","addr":"192.168.1.1:54321","connected":"2026-07-24T10:30:00+08:00"}]`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStatus(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "node-1") {
		t.Errorf("expected output to contain node-1, got: %s", buf.String())
	}
}

func TestRelayStatusCmd_Empty(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStatus(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "暂无已连接节点") {
		t.Errorf("expected empty message, got: %s", buf.String())
	}
}

func TestBuildRelayHandler_HappyPath(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("backend response"))
	}))
	defer backend.Close()

	handler := buildRelayHandler(context.Background(), backend.URL, http.DefaultClient, testutil.DiscardLogger())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "backend response") {
		t.Errorf("expected body 'backend response', got %s", rec.Body.String())
	}
}

func TestBuildRelayHandler_BackendUnreachable(t *testing.T) {
	t.Parallel()
	handler := buildRelayHandler(context.Background(), "http://127.0.0.1:1", http.DefaultClient, testutil.DiscardLogger())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}
}

func TestBuildRelayHandler_QueryParams(t *testing.T) {
	t.Parallel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "key=val" {
			t.Errorf("expected query 'key=val', got %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer backend.Close()

	handler := buildRelayHandler(context.Background(), backend.URL, http.DefaultClient, testutil.DiscardLogger())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test?key=val", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestRunRelayWithRetry_CtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runRelayWithRetry(ctx, "test-node", "ws://hub", "http://local", "", false, nil, nil, testutil.DiscardLogger())
	// With cancelled context, runRelayOnce will fail quickly (ws dial fails),
	// then runRelayWithRetry returns the error (ctx.Err() != nil)
	if err == nil {
		t.Fatal("expected error after context cancellation")
	}
}

// TestIsTerminalRelayError 验证鉴权错误识别只采信 hub 显式 REG_ERR 帧（"注册失败"）。
// EOF / 连接断开 / 超时等网络波动不得被当作配置/鉴权错误——它们应继续重连。
func TestIsTerminalRelayError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"hub 明确拒绝 invalid token（REG_ERR 帧）", fmt.Errorf("注册失败: invalid token"), true},
		{"hub 明确拒绝 bad register frame", fmt.Errorf("注册失败: bad register frame"), true},
		{"未知注册响应", fmt.Errorf("注册失败: 收到未知注册响应 %q", "???"), true},
		// 网络波动：不得判为终态
		{"等待 ACK 超时（EOF）", fmt.Errorf("等待注册 ACK 失败: %w", io.EOF), false},
		{"连接断开 EOF", io.EOF, false},
		{"连接到 Hub 失败", fmt.Errorf("连接到 Hub 失败: %w", io.ErrClosedPipe), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalRelayError(tc.err); got != tc.want {
				t.Fatalf("isTerminalRelayError() = %v, want %v (err=%v)", got, tc.want, tc.err)
			}
		})
	}
}

// TestParseRegisterAck 验证注册 ACK 三段解析（I1）：
// REG_OK 纯串 / REG_OK:<secret> / REG_ERR:<msg> 三种形态正确分类。
// 回归锁：声明 per-node-secret 能力后 hub 回 "REG_OK:<base64url secret>"，
// 若用精确比较会误判为未知响应导致 relay start 终止（B1 复检 bug）。
func TestParseRegisterAck(t *testing.T) {
	secret, err := parseRegisterAck(hub.RegisterAckOK)
	if err != nil || secret != "" {
		t.Fatalf("expected REG_OK to no secret, got secret=%q err=%v", secret, err)
	}

	const wantSecret = "abc123"
	secret, err = parseRegisterAck(hub.RegisterAckOK + ":" + wantSecret)
	if err != nil {
		t.Fatalf("expected REG_OK:secret parse success, got %v", err)
	}
	if secret != wantSecret {
		t.Fatalf("expected secret %q, got %q", wantSecret, secret)
	}
	// REG_OK:secret 不是终态错误（不终止重连）
	if isTerminalRelayError(err) {
		t.Fatal("REG_OK:secret 不应被 isTerminalRelayError 判为终态")
	}

	_, err = parseRegisterAck(hub.RegisterAckErr + "invalid token")
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("expected REG_ERR error containing reason, got %v", err)
	}
	if !isTerminalRelayError(err) {
		t.Fatal("REG_ERR 应被 isTerminalRelayError 判为终态")
	}

	_, err = parseRegisterAck(hub.RegisterAckOK + ":")
	if err == nil {
		t.Fatal("expected error for empty secret after REG_OK:")
	}
	if !isTerminalRelayError(err) {
		t.Fatal("异常 REG_OK（secret 为空）应判为终态")
	}

	_, err = parseRegisterAck("???")
	if err == nil || !strings.Contains(err.Error(), "未知注册响应") {
		t.Fatalf("expected unknown-response error, got %v", err)
	}
}

func TestRunRelayStart_AutoNodeID(t *testing.T) {
	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	// Check default flag values
	hub, _ := cmd.Flags().GetString("hub")
	if hub != "ws://127.0.0.1:18084/ws" {
		t.Errorf("expected default hub flag, got %q", hub)
	}
	local, _ := cmd.Flags().GetString("local")
	if local != "http://127.0.0.1:8080" {
		t.Errorf("expected default local flag, got %q", local)
	}
	nodeID, _ := cmd.Flags().GetString("node-id")
	if nodeID != "" {
		t.Errorf("expected empty node-id, got %q", nodeID)
	}
}

func TestRunRelayStart_EmptyHubURL(t *testing.T) {
	// Use a short timeout context to prevent the retry loop from hanging
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	_ = cmd.Flags().Set("hub", "")
	cmd.SetContext(ctx)
	err := cmd.RunE(cmd, []string{})
	if err == nil {
		t.Error("expected error when hub URL is empty")
	}
}

func TestRelayStopCmd(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdRelayStop(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "SIGINT") {
		t.Errorf("expected output to contain SIGINT, got: %s", buf.String())
	}
}
