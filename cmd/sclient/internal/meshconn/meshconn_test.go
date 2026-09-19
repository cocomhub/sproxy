// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meshconn

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// newTestCmd 构造带 AddFlags + AddExitFlags 注册的命令（测 flag 可读性与互斥校验）。
func newTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	AddFlags(cmd)
	AddExitFlags(cmd)
	return cmd
}

func TestAddFlags_Registered(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	for _, name := range []string{
		"exit", "exit-auto", "exit-only", "exit-exclude", "local-timeout",
		"gateway", "smart", "smart-ttl", "mdns", "mdns-secret",
		"webrtc", "hub", "node-id", "insecure", "stun", "turn",
		"turn-user", "turn-pass",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("缺少 flag: --%s", name)
		}
	}
}

func TestFromFlags_Defaults(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if conn.LocalTimeout <= 0 {
		t.Fatalf("LocalTimeout = %v, want 默认 3s", conn.LocalTimeout)
	}
	if conn.ExitNode != "" || conn.ExitAuto {
		t.Fatalf("默认不应有出口: %+v", conn)
	}
}

func TestFromFlags_ExitAutoExclusive(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-auto", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit 与 --exit-auto 应互斥报错")
	}
}

func TestFromFlags_ExitExcludeRequiresAuto(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-exclude", "node-y")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("固定 --exit 时 --exit-exclude 应 fail-closed 报错")
	}
}

func TestFromFlags_ExitOnlyRequiresExit(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-only", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit-only 无出口候选应 fail-closed 报错")
	}
}

// stubConfigProvider 注入固定配置（测 stun/turn 配置回落）。
type stubConfigProvider struct {
	cfg *client.Config
}

func (s *stubConfigProvider) LoadConfig() (*client.Config, error) { return s.cfg, nil }

func TestFromFlags_ConfigFallback_STUN_TURN(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	cfg := client.DefaultConfig()
	cfg.STUNServers = []string{"stun:stub:3478"}
	cfg.TURNServers = []string{"turn:stub:3478"}
	cfg.TURNUser = "u"
	cfg.TURNPass = "p"
	conn := &Conn{}
	if err := conn.FromFlags(cmd, &stubConfigProvider{cfg: cfg}); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.STUN) != 1 || conn.STUN[0] != "stun:stub:3478" {
		t.Fatalf("STUN 配置回落失败: %v", conn.STUN)
	}
	if len(conn.TURN) != 1 || conn.TURN[0] != "turn:stub:3478" {
		t.Fatalf("TURN 配置回落失败: %v", conn.TURN)
	}
	if conn.TURNUser != "u" || conn.TURNPass != "p" {
		t.Fatalf("TURN 凭据配置回落失败: %+v", conn)
	}
}

func TestFromFlags_ConfigFallback_FlagOverrides(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("stun", "stun:flag:3478")
	cfg := client.DefaultConfig()
	cfg.STUNServers = []string{"stun:cfg:3478"}
	conn := &Conn{}
	if err := conn.FromFlags(cmd, &stubConfigProvider{cfg: cfg}); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.STUN) != 1 || conn.STUN[0] != "stun:flag:3478" {
		t.Fatalf("flag 应优先于配置回落: %v", conn.STUN)
	}
}

func TestLocalOrExit_ExitOnly_UsesExitDial(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalled.Add(1)
		return nil, net.ErrClosed
	}
	conn := &Conn{ExitOnly: true, LocalTimeout: 3 * time.Second}
	dial := conn.LocalOrExit(exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err != net.ErrClosed {
		t.Fatalf("err = %v, want net.ErrClosed（恒出口应直接用 exitDial）", err)
	}
	if exitCalled.Load() != 1 {
		t.Fatalf("exitDial 调用 %d 次, want 1", exitCalled.Load())
	}
}

func TestLocalOrExit_ZeroTimeout_UsesExitDial(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalled.Add(1)
		return nil, net.ErrClosed
	}
	conn := &Conn{LocalTimeout: 0} // 0 = 不试本地
	dial := conn.LocalOrExit(exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err != net.ErrClosed {
		t.Fatalf("err = %v, want net.ErrClosed（0 超时应直接用 exitDial）", err)
	}
	if exitCalled.Load() != 1 {
		t.Fatalf("exitDial 调用 %d 次, want 1", exitCalled.Load())
	}
}

func TestLocalOrExit_NilExitDial_LocalOnly(t *testing.T) {
	t.Parallel()
	conn := &Conn{LocalTimeout: 3 * time.Second}
	dial := conn.LocalOrExit(nil)
	if dial == nil {
		t.Fatalf("LocalOrExit(nil) 应返回本地直连函数（非 nil）")
	}
	// 本地直连不可达目标 → 报错（证明走 net.Dialer 而非 panic）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dial(ctx, "127.0.0.1:1"); err == nil {
		t.Fatalf("本地直连不可达目标应报错")
	}
}

func TestNormalizeListen(t *testing.T) {
	t.Parallel()
	if got := NormalizeListen(":1080"); got != "127.0.0.1:1080" {
		t.Fatalf("NormalizeListen(:1080) = %q, want 127.0.0.1:1080", got)
	}
	if got := NormalizeListen("127.0.0.1:1080"); got != "127.0.0.1:1080" {
		t.Fatalf("NormalizeListen 不应改动显式地址: %q", got)
	}
}
