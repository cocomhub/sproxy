// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// fakeSwitchMux 提供可控 Retransmits/FramesSent 的 mux 指标源。
type fakeSwitchMux struct {
	sent   atomic.Int64
	retx   atomic.Int64
	closed atomic.Bool
}

func (f *fakeSwitchMux) Metrics() *mux.Metrics {
	mm := &mux.Metrics{}
	mm.FramesSent.Store(f.sent.Load())
	mm.Retransmits.Store(f.retx.Load())
	return mm
}

func (f *fakeSwitchMux) Close() error { f.closed.Store(true); return nil }

// fakeSwitchConn 是质量切换的旧连接（MuxStreamConn 兼容——需 MuxOfResult 可提取）。
type fakeSwitchConn struct{ f *fakeSwitchMux }

func (c *fakeSwitchConn) Read(p []byte) (int, error)         { return 0, nil }
func (c *fakeSwitchConn) Write(p []byte) (int, error)        { return 0, nil }
func (c *fakeSwitchConn) Close() error                       { return c.f.Close() }
func (c *fakeSwitchConn) LocalAddr() net.Addr                { return nil }
func (c *fakeSwitchConn) RemoteAddr() net.Addr               { return nil }
func (c *fakeSwitchConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeSwitchConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeSwitchConn) SetWriteDeadline(t time.Time) error { return nil }

// 红灯：质量触发动态切换未实现——Monitor 不存在（编译红）。

// TestQualitySwitch_DetectsDegradation 劣化检测：重传率持续超阈值 → 判劣化。
func TestQualitySwitch_DetectsDegradation(t *testing.T) {
	// sproxy:serial: 包级质量注册表/指标全局，不可并行
	qualityRegistryClear()
	t.Cleanup(qualityRegistryClear)
	m := &fakeSwitchMux{}
	m.sent.Store(100)
	m.retx.Store(50) // 重传率 0.5 > 0.1

	mon := NewQualitySwitchMonitor(QualitySwitchConfig{Threshold: 0.1, Samples: 3, Cooldown: time.Second}, m.Metrics)
	if got := mon.degraded(); !got {
		t.Fatalf("重传率 0.5 应判劣化, got %v", got)
	}
}

// TestQualitySwitch_HealthyNoSwitch 健康连接（重传率 0）→ 不判劣化。
func TestQualitySwitch_HealthyNoSwitch(t *testing.T) {
	t.Parallel()
	qualityRegistryClear()
	t.Cleanup(qualityRegistryClear)
	m := &fakeSwitchMux{}
	m.sent.Store(100)
	mon := NewQualitySwitchMonitor(QualitySwitchConfig{Threshold: 0.1, Samples: 3}, m.Metrics)
	if got := mon.degraded(); got {
		t.Fatalf("重传率 0 不应判劣化, got %v", got)
	}
}

// TestQualitySwitch_TrySwitchRedials 劣化触发切换：redial 被调 + 旧连接关闭 + 计数。
func TestQualitySwitch_TrySwitchRedials(t *testing.T) {
	// sproxy:serial: 包级计数器全局
	qualitySwitchCounterClear()
	t.Cleanup(qualitySwitchCounterClear)
	m := &fakeSwitchMux{}
	m.sent.Store(100)
	m.retx.Store(50)

	var redialCalls atomic.Int64
	var oldClosed atomic.Bool
	mon := NewQualitySwitchMonitor(QualitySwitchConfig{Threshold: 0.1, Samples: 1, Cooldown: time.Second}, m.Metrics).
		WithRedial(func(ctx context.Context) (*Result, error) {
			redialCalls.Add(1)
			return &Result{Kind: KindRelay, Conn: &fakeSwitchConn{f: m}}, nil
		}).
		WithCloseOld(func() error { oldClosed.Store(true); return nil })
	mon.checkOnce(context.Background())

	if redialCalls.Load() != 1 {
		t.Fatalf("劣化应触发 1 次重拨, got %d", redialCalls.Load())
	}
	if !oldClosed.Load() {
		t.Fatalf("切换成功后旧连接应关闭")
	}
	if got := QualitySwitchCounters()["degraded"]; got != 1 {
		t.Fatalf("切换计数 degraded 应 1, got %d", got)
	}
}

// TestQualitySwitch_CooldownSkips 冷却期内不重复切换。
func TestQualitySwitch_CooldownSkips(t *testing.T) {
	t.Parallel()
	qualitySwitchCounterClear()
	t.Cleanup(qualitySwitchCounterClear)
	m := &fakeSwitchMux{}
	m.sent.Store(100)
	m.retx.Store(50)

	var redialCalls atomic.Int64
	mon := NewQualitySwitchMonitor(QualitySwitchConfig{Threshold: 0.1, Samples: 1, Cooldown: time.Hour}, m.Metrics).
		WithRedial(func(ctx context.Context) (*Result, error) {
			redialCalls.Add(1)
			return &Result{Kind: KindRelay}, nil
		})
	mon.checkOnce(context.Background())
	mon.checkOnce(context.Background()) // 冷却内第二次
	if redialCalls.Load() != 1 {
		t.Fatalf("冷却内应只切换 1 次, got %d", redialCalls.Load())
	}
}

// TestQualitySwitch_LockDisables 手动锁定：不触发切换（即使劣化）。
func TestQualitySwitch_LockDisables(t *testing.T) {
	t.Parallel()
	qualitySwitchCounterClear()
	t.Cleanup(qualitySwitchCounterClear)
	m := &fakeSwitchMux{}
	m.sent.Store(100)
	m.retx.Store(50)

	var redialCalls atomic.Int64
	mon := NewQualitySwitchMonitor(QualitySwitchConfig{Threshold: 0.1, Samples: 1, Cooldown: time.Second, LockTransport: true}, m.Metrics).
		WithRedial(func(ctx context.Context) (*Result, error) {
			redialCalls.Add(1)
			return &Result{Kind: KindRelay}, nil
		})
	mon.checkOnce(context.Background())
	if redialCalls.Load() != 0 {
		t.Fatalf("手动锁定应禁用切换, got %d 次重拨", redialCalls.Load())
	}
}

// TestQualitySwitch_DefaultOffZeroRegression 默认关（无 Monitor 装配）= 无副作用。
func TestQualitySwitch_DefaultOffZeroRegression(t *testing.T) {
	t.Parallel()
	if got := QualitySwitchCounters(); len(got) != 0 {
		t.Fatalf("默认无切换计数, got %v", got)
	}
}

// TestQualitySwitch_MetricsOfNonMuxNil 非 mux 连接 → 指标采样 nil（监控跳过）。
func TestQualitySwitch_MetricsOfNonMuxNil(t *testing.T) {
	t.Parallel()
	_ = fakeSwitchConn{f: &fakeSwitchMux{}} // 编译断言：满足接口
	// 非 mux 连接（无 Mux() 方法）→ nil conn 场景验证。
	if got := MuxMetricsOfConn(nil); got != nil {
		t.Fatalf("nil conn 应返回 nil 采样, got %v", got)
	}
}
