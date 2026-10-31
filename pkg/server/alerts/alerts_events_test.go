// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// alerts_events_test.go 覆盖事件源挂点与分发细节：
//  1. sync_failed / login_locked / checksum_mismatch 事件 → 告警（FIRING 去抖 +
//     消息字段 Action/Object）。
//  2. 同 source 多规则：状态机按 source+key 去抖，「同 source+key 只发一次通知」
//     （已 firing 时后续规则不再重复通知）。
//  3. channelsFor：规则渠道名未注册 / 同名渠道重复注册（Register 返回 false 不覆盖）。
//  4. dispatch：渠道 Send 失败只记 Warn（不重试、不吞其它渠道）；splitKey 的异常形态。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/server/notify"
)

// messageRecorder 是记录完整 NotifyMessage 的假渠道（可注入 Send 错误）。
type messageRecorder struct {
	name string
	msgs chan notify.NotifyMessage
	err  error // Send 返回值（非 nil 模拟渠道故障）
}

func newMessageRecorder(name string) *messageRecorder {
	return &messageRecorder{name: name, msgs: make(chan notify.NotifyMessage, 16)}
}

func (r *messageRecorder) Name() string { return r.name }

func (r *messageRecorder) Send(ctx context.Context, m notify.NotifyMessage) error {
	r.msgs <- m
	return r.err
}

// recv 等待一条通知（超时即测试失败）。
func (r *messageRecorder) recv(t *testing.T) notify.NotifyMessage {
	t.Helper()
	select {
	case m := <-r.msgs:
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("渠道 %s 应收到通知", r.name)
		return notify.NotifyMessage{}
	}
}

// expectSilent 断言在给定窗口内没有通知。
func (r *messageRecorder) expectSilent(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.msgs:
		t.Fatalf("渠道 %s 不应收到通知，got %+v", r.name, m)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestEngine_EventSources 事件源挂点：触发 → 告警（字段正确）→ 同 key 去抖。
func TestEngine_EventSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		source   string
		trigger  func(ctx context.Context, e *Engine)
		object   string
		wantText []string
	}{
		{
			name:     "sync_failed",
			source:   "sync_failed",
			trigger:  func(ctx context.Context, e *Engine) { e.OnSyncFailed(ctx, "task-1", "remote rejected") },
			object:   "task-1",
			wantText: []string{"task-1", "remote rejected"},
		},
		{
			name:     "login_locked",
			source:   "login_locked",
			trigger:  func(ctx context.Context, e *Engine) { e.OnLoginLocked(ctx, "AK-EXAMPLE-1") },
			object:   "AK-EXAMPLE-1",
			wantText: []string{"AK-EXAMPLE-1", "锁定"},
		},
		{
			name:     "checksum_mismatch",
			source:   "checksum_mismatch",
			trigger:  func(ctx context.Context, e *Engine) { e.OnChecksumMismatch(ctx, "alice", 3, "segment 7") },
			object:   "alice",
			wantText: []string{"3", "alice", "segment 7"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := newMessageRecorder("wecom")
			eng := NewEngine(config.AlertConfig{
				Enabled: true,
				Rules:   []config.AlertRule{{Source: tc.source, Channels: []string{"wecom"}}},
			}, testLogger())
			eng.Register(ch)
			t.Cleanup(eng.Close)
			ctx := context.Background()

			tc.trigger(ctx, eng)
			msg := ch.recv(t)
			if msg.Action != tc.source {
				t.Fatalf("Action 应为 source %q，got %q", tc.source, msg.Action)
			}
			if msg.Object != tc.object {
				t.Fatalf("Object 应为 %q，got %q", tc.object, msg.Object)
			}
			if want := "[sproxy] 告警: " + tc.source; msg.Title != want {
				t.Fatalf("Title 应为 %q，got %q", want, msg.Title)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(msg.Text, want) {
					t.Fatalf("告警正文应含 %q：%s", want, msg.Text)
				}
			}

			// 同 key 重复事件 → 去抖（state=firing 不再通知）。
			tc.trigger(ctx, eng)
			ch.expectSilent(t)
		})
	}
}

// TestEngine_MultipleRulesSameSourceSingleNotification 同 source 多规则：
// 状态机按 source+key 去抖（生产注释约定「同 source+key 只发一次通知」），
// 因此第一条规则 fire 之后，第二条规则不再重复通知；恢复通知同样只由首条规则发出。
func TestEngine_MultipleRulesSameSourceSingleNotification(t *testing.T) {
	t.Parallel()
	first := newMessageRecorder("first")
	second := newMessageRecorder("second")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules: []config.AlertRule{
			{Source: "volume_degraded", Channels: []string{"first"}},
			{Source: "volume_degraded", Channels: []string{"second"}},
		},
	}, testLogger())
	eng.Register(first)
	eng.Register(second)
	t.Cleanup(eng.Close)

	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	first.recv(t)
	second.expectSilent(t)

	eng.OnVolumeRecovered(context.Background(), "vol1")
	first.recv(t)
	second.expectSilent(t)
}

// TestEngine_ChannelSelection 渠道解析：未注册的渠道名被跳过（不 panic、不外发），
// 同名渠道重复注册被拒且不覆盖既有实例。
func TestEngine_ChannelSelection(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules: []config.AlertRule{{
			Source:   "volume_degraded",
			Channels: []string{"slack-unregistered", "wecom"},
		}},
	}, testLogger())
	if !eng.Register(ch) {
		t.Fatal("首次注册应成功")
	}
	dup := newMessageRecorder("wecom")
	if eng.Register(dup) {
		t.Fatal("同名渠道重复注册应返回 false")
	}
	t.Cleanup(eng.Close)

	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	ch.recv(t) // 已注册渠道收到
	dup.expectSilent(t)
}

// TestEngine_DispatchSendFailureLogged 渠道 Send 失败：记 Warn 日志、继续投递其它渠道、
// 且同 key 再次触发仍走去抖（同步单次，不重试）。
func TestEngine_DispatchSendFailureLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	failing := newMessageRecorder("flaky")
	failing.err = errors.New("webhook 500")
	ok := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{
		Enabled: true,
		Rules: []config.AlertRule{{
			Source:   "volume_degraded",
			Channels: []string{"flaky", "wecom"},
		}},
	}, logger)
	eng.Register(failing)
	eng.Register(ok)
	t.Cleanup(eng.Close)

	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	ok.recv(t)
	select {
	case <-failing.msgs:
	default:
		t.Fatal("失败渠道也应被调用一次")
	}

	logLine := buf.String()
	for _, want := range []string{"告警发送失败", "flaky", "webhook 500"} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("失败应记 Warn 日志（含 %q）：%s", want, logLine)
		}
	}

	// 失败不重试：同 key 再触发即去抖，失败渠道不再被调用。
	eng.OnVolumeDegraded(context.Background(), "vol1", "probe failed")
	select {
	case <-failing.msgs:
		t.Fatal("同 key 去抖后不应重发到失败渠道")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSplitKey 覆盖 key 拆分的全部形态（含无分隔符/空串）。
func TestSplitKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want [2]string
	}{
		{name: "分隔符在中间", key: "volume_degraded\x00vol1", want: [2]string{"volume_degraded", "vol1"}},
		{name: "对象为空", key: "disk_watermark\x00", want: [2]string{"disk_watermark", ""}},
		{name: "无分隔符", key: "disk_watermark", want: [2]string{"disk_watermark", ""}},
		{name: "空串", key: "", want: [2]string{"", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := splitKey(tc.key)
			if len(got) != 2 {
				t.Fatalf("splitKey 应返回 2 段，got %#v", got)
			}
			if got[0] != tc.want[0] || got[1] != tc.want[1] {
				t.Fatalf("splitKey(%q) = %#v，want %#v", tc.key, got, tc.want)
			}
		})
	}
}

// TestEngine_DispatchKeyWithoutSeparator key 无分隔符时：标题/Action 取整串、Object 为空。
func TestEngine_DispatchKeyWithoutSeparator(t *testing.T) {
	t.Parallel()
	ch := newMessageRecorder("wecom")
	eng := NewEngine(config.AlertConfig{Enabled: true}, testLogger())
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.dispatch(context.Background(), []notify.Notifier{ch}, "disk_watermark", "磁盘水位 95%")
	msg := ch.recv(t)
	if want := "[sproxy] 告警: disk_watermark"; msg.Title != want {
		t.Fatalf("Title 应为 %q，got %q", want, msg.Title)
	}
	if msg.Action != "disk_watermark" {
		t.Fatalf("Action 应为 disk_watermark，got %q", msg.Action)
	}
	if msg.Object != "" {
		t.Fatalf("无分隔符时 Object 应为空，got %q", msg.Object)
	}
	if msg.Text != "磁盘水位 95%" {
		t.Fatalf("未装配 advisor 时正文应原样，got %q", msg.Text)
	}
}
