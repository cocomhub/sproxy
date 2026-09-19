// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// startEcho 是一个可拨通的回显监听器（供桩 Dial 返回真实连接）。
// accept 到连接后立即关闭——只需验证「拨通」，不需要数据往返。
func startEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestLocalOrExitDial_LocalSucceeds_NoExit(t *testing.T) {
	ln := startEcho(t)
	var exitCalls atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalls.Add(1)
		return nil, errors.New("exit should not be called")
	}
	dial := NewLocalOrExitDial(500*time.Millisecond, exit)
	// 本地直连到 echo：本地拨号成功时 exit 不应被调用。
	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("本地直连失败: %v", err)
	}
	_ = conn.Close()
	if exitCalls.Load() != 0 {
		t.Fatalf("exit 被调用 %d 次, want 0（本地成功不应回退）", exitCalls.Load())
	}
}

func TestLocalOrExitDial_LocalTimeout_FallsBackToExit(t *testing.T) {
	// 本地直连目标不可达（127.0.0.1:1 拒绝），应回退 exit。
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		ln := startEcho(t)
		return net.Dial("tcp", ln.Addr().String())
	}
	dial := NewLocalOrExitDial(300*time.Millisecond, exit)
	conn, err := dial(context.Background(), "127.0.0.1:1") // 本地拒绝
	if err != nil {
		t.Fatalf("回退 exit 失败: %v", err)
	}
	_ = conn.Close()
}

func TestLocalOrExitDial_ExitFails_Propagates(t *testing.T) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, errors.New("exit down")
	}
	dial := NewLocalOrExitDial(100*time.Millisecond, exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "exit down") {
		t.Fatalf("err = %v, want 含 exit down", err)
	}
}

func TestLocalOrExitDial_ZeroTimeout_NoLocal(t *testing.T) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		ln := startEcho(t)
		return net.Dial("tcp", ln.Addr().String())
	}
	dial := NewLocalOrExitDial(0, exit) // 0 = 不试本地
	conn, err := dial(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("直接 exit: %v", err)
	}
	_ = conn.Close()
}

func TestLocalOrExitDial_NilExit_LocalOnly(t *testing.T) {
	// exit 为 nil：退化为纯本地直连（本机出口语义），不可达目标报错而非 panic。
	ln := startEcho(t)
	dial := NewLocalOrExitDial(300*time.Millisecond, nil)
	conn, err := dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("本地直连失败: %v", err)
	}
	_ = conn.Close()
	if _, err := dial(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatalf("不可达目标应报错")
	}
}

func TestAutoExitDial_ExcludesOutboundDial(t *testing.T) {
	nodes := []client.HubNodeInfo{
		{ID: "exit-a", Capabilities: []string{"outbound-dial"}},
		{ID: "exit-b", Capabilities: []string{"outbound-dial"}},
		{ID: "relay-only", Capabilities: []string{}},
	}
	var called []string
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			called = append(called, nodeID)
			if nodeID == "exit-a" {
				// exit-a 拨号失败 → 若 exclude 生效应跳过 exit-b 整体失败；
				// 若 exclude 失效 exit-b 会被尝试并成功（变异被抓）。
				return nil, errors.New("exit-a down")
			}
			ln := startEcho(t)
			return net.Dial("tcp", ln.Addr().String())
		}
	}
	dial := NewAutoExitDial(0, func(ctx context.Context) ([]client.HubNodeInfo, error) { return nodes, nil }, exitDialFor, []string{"exit-b"})
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Fatalf("exit-b 被排除，全部候选不可达应报错（called=%v）", called)
	}
	if len(called) != 1 || called[0] != "exit-a" {
		t.Fatalf("called = %v, want [exit-a]（exit-b 被排除，relay-only 无 outbound-dial）", called)
	}
}

func TestAutoExitDial_NoOutboundDial_FallsBackToAll(t *testing.T) {
	// 无 outbound-dial 节点：回落全部在线节点减 exclude。
	nodes := []client.HubNodeInfo{
		{ID: "plain-1", Capabilities: []string{}},
		{ID: "plain-2", Capabilities: []string{}},
	}
	var called []string
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			called = append(called, nodeID)
			ln := startEcho(t)
			return net.Dial("tcp", ln.Addr().String())
		}
	}
	dial := NewAutoExitDial(0, func(ctx context.Context) ([]client.HubNodeInfo, error) { return nodes, nil }, exitDialFor, []string{"plain-2"})
	conn, err := dial(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("自动选出口失败: %v", err)
	}
	_ = conn.Close()
	if len(called) != 1 || called[0] != "plain-1" {
		t.Fatalf("called = %v, want [plain-1]（无 outbound-dial 回落全部，plain-2 被 exclude）", called)
	}
}

func TestAutoExitDial_AllCandidatesFail_Propagates(t *testing.T) {
	nodes := []client.HubNodeInfo{
		{ID: "exit-a", Capabilities: []string{"outbound-dial"}},
		{ID: "exit-b", Capabilities: []string{"outbound-dial"}},
	}
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errors.New("candidate down: " + nodeID)
		}
	}
	dial := NewAutoExitDial(0, func(ctx context.Context) ([]client.HubNodeInfo, error) { return nodes, nil }, exitDialFor, nil)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "全部候选不可达") {
		t.Fatalf("err = %v, want 含 全部候选不可达", err)
	}
}

func TestAutoExitDial_NodeListerFails_Propagates(t *testing.T) {
	exitDialFor := func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error) {
		return func(ctx context.Context, addr string) (net.Conn, error) { return nil, errors.New("unused") }
	}
	dial := NewAutoExitDial(0, func(ctx context.Context) ([]client.HubNodeInfo, error) {
		return nil, errors.New("hub unreachable")
	}, exitDialFor, nil)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "拉取节点列表失败") {
		t.Fatalf("err = %v, want 含 拉取节点列表失败", err)
	}
}
