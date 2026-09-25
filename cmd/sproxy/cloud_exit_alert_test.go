// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// cloud_exit_alert_test.go 验证 NAT 穿透失败告警的装配层包装（roadmap 11.1-①）：
//   - withNATAlert：拨号失败 → OnNATFailure（peer + 错误详情）；成功 → OnNATRecovered；
//     **错误原样向上传播**（fail-closed 语义不变，告警是旁路副作用）；
//   - e == nil（告警引擎未启用）→ 直接返回原 dial（零开销、行为零变化）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
)

// alertTestChannel 记录告警消息的假渠道（cmd/sproxy 侧：实现 server.Notifier
// 接口，替代 pkg/server 包内同名的 newFakeAlertChannel）。
type alertTestChannel struct {
	name string
	msgs chan string
}

func newAlertTestChannel(name string) *alertTestChannel {
	return &alertTestChannel{name: name, msgs: make(chan string, 64)}
}

func (f *alertTestChannel) Name() string { return f.name }

func (f *alertTestChannel) Send(_ context.Context, m server.NotifyMessage) error {
	f.msgs <- m.Title + "|" + m.Text
	return nil
}

// testLogger 是 cmd/sproxy 包的丢弃日志器（与本包其它测试同款）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestWithNATAlert_FailureFiresAndPropagates 拨号失败：告警引擎收到失败事件
// （含 peer 与错误详情），且错误原样向上传播（不吞错、不改拨号语义）。
func TestWithNATAlert_FailureFiresAndPropagates(t *testing.T) {
	t.Parallel()
	ch := newAlertTestChannel("wecom")
	eng := server.NewAlertEngine(server.AlertConfig{
		Enabled: true,
		Rules:   []server.AlertRule{{Source: server.SourceNATFailure, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	wantErr := errors.New("relay stream down")
	dial := withNATAlert(func(context.Context, string) (net.Conn, error) {
		return nil, wantErr
	}, eng, "node-exit")

	conn, err := dial(context.Background(), "http://example.com/file")
	if conn != nil {
		t.Fatalf("失败时不应返回连接")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("拨号错误必须原样传播（fail-closed），got %v", err)
	}
	select {
	case m := <-ch.msgs:
		if !strings.Contains(m, "node-exit") || !strings.Contains(m, "relay stream down") {
			t.Fatalf("告警应含 peer 与错误详情: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("拨号失败应触发 NAT 失败告警")
	}
}

// TestWithNATAlert_SuccessRecovers 拨号成功：告警引擎收到恢复事件（同 peer）。
// 先失败再成功 → firing → ok（恢复通知）。
func TestWithNATAlert_SuccessRecovers(t *testing.T) {
	t.Parallel()
	ch := newAlertTestChannel("wecom")
	eng := server.NewAlertEngine(server.AlertConfig{
		Enabled: true,
		Rules:   []server.AlertRule{{Source: server.SourceNATFailure, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	calls := 0
	inner := func(context.Context, string) (net.Conn, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	dial := withNATAlert(inner, eng, "node-exit")

	if _, err := dial(context.Background(), "addr"); err == nil {
		t.Fatal("首次应失败")
	}
	select {
	case <-ch.msgs:
	default:
		t.Fatal("首次失败应发告警")
	}
	conn, err := dial(context.Background(), "addr")
	if err != nil {
		t.Fatalf("第二次应成功: %v", err)
	}
	if conn == nil {
		t.Fatal("成功时不应返回 nil 连接")
	}
	select {
	case <-ch.msgs:
	default:
		t.Fatal("后续成功应发恢复通知")
	}
}

// TestWithNATAlert_NilEngineNoOp e == nil（告警引擎未启用）→ 直接返回原 dial：
// 错误原样传播、无任何副作用（零开销零变化）。
func TestWithNATAlert_NilEngineNoOp(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("dial down")
	inner := func(context.Context, string) (net.Conn, error) { return nil, wantErr }
	if got := withNATAlert(inner, nil, "peer-x"); got == nil {
		t.Fatal("e==nil 时应返回原 dial（非 nil）")
	} else {
		if _, err := got(context.Background(), "addr"); !errors.Is(err, wantErr) {
			t.Fatalf("nil engine 时错误应原样传播: %v", err)
		}
	}
	if got := withNATAlert(nil, server.NewAlertEngine(server.AlertConfig{}, testLogger()), "peer-x"); got != nil {
		t.Fatal("dial==nil 时应返回 nil（不包装）")
	}
}
