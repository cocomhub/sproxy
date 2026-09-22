// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// 质量触发动态切换（roadmap 5.3 P2）：运行中连接质量劣化检测 + 自动重拨切换。
//
// 语义：
//   - 周期采样当前连接 mux 的重传率（复用 #467 质量源 QualityOf/RegisterMuxQuality 同源指标）；
//     重传率持续 N 个采样周期超过阈值 → 判劣化。
//   - 劣化 → 用 DialSmartWithOptions 重拨（SmartDial 竞速选更优候选）→ 成功切换（旧连接关闭）。
//   - 防抖：切换后冷却期内不重复切换（默认 30s）。
//   - 手动锁定 LockTransport=true：禁用自动切换（安全开关可观测——日志记录锁定态）。
//
// 默认关零回归：装配层不创建 Monitor = 行为不变；LockTransport 默认 false。

// QualitySwitchConfig 是质量切换监控配置（显式开关；零值 = 全默认）。
type QualitySwitchConfig struct {
	// Threshold 是劣化重传率阈值（默认 0.1）；0 = 默认。
	Threshold float64
	// Samples 是连续超阈值判劣化的采样周期数（默认 3）；0 = 默认。
	Samples int
	// Cooldown 是切换后冷却时长（默认 30s）；0 = 默认。
	Cooldown time.Duration
	// LockTransport 是手动锁定：true 禁用自动切换（安全可观测——日志记录）。
	LockTransport bool
	// Interval 是采样间隔（默认 5s）；0 = 默认。测试可设小值加速。
	Interval time.Duration
}

func (c QualitySwitchConfig) withDefaults() QualitySwitchConfig {
	if c.Threshold <= 0 {
		c.Threshold = 0.1
	}
	if c.Samples <= 0 {
		c.Samples = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 30 * time.Second
	}
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	return c
}

// qualitySwitchRecorder 是切换事件回调（metrics/日志注入；nil = 不记录零回归）。
type qualitySwitchRecorder func(reason, fromID, toID string)

// qualitySwitchCounters 是切换计数（sproxy_mesh_switch_total{reason} 的进程内聚合；
// 装配层拉取后进 /metrics）。
var qualitySwitchCounters = struct {
	mu      sync.Mutex
	reasons map[string]int64
}{reasons: make(map[string]int64)}

// RecordQualitySwitch 记录一次切换（按 reason 计数，供 /metrics 聚合）。
func RecordQualitySwitch(reason string) {
	qualitySwitchCounters.mu.Lock()
	defer qualitySwitchCounters.mu.Unlock()
	qualitySwitchCounters.reasons[reason]++
}

// QualitySwitchCounters 返回切换计数快照（map 拷贝；无切换 = 空 map）。
func QualitySwitchCounters() map[string]int64 {
	qualitySwitchCounters.mu.Lock()
	defer qualitySwitchCounters.mu.Unlock()
	out := make(map[string]int64, len(qualitySwitchCounters.reasons))
	maps.Copy(out, qualitySwitchCounters.reasons)
	return out
}

// qualitySwitchCounterClear 清空切换计数（仅测试用）。
func qualitySwitchCounterClear() {
	qualitySwitchCounters.mu.Lock()
	defer qualitySwitchCounters.mu.Unlock()
	qualitySwitchCounters.reasons = make(map[string]int64)
}

// qualityMetricsFn 是质量指标采样函数（返回当前 mux 指标；nil = 无数据）。
// 接口化而非直接依赖 *mux.Mux：测试注入 fake、装配层传 MuxQualitySource.QualityMetrics。
type qualityMetricsFn func() *mux.Metrics

// QualitySwitchMonitor 是运行中连接的质量劣化监控器。
// 装配层创建后 Run（阻塞；ctx 取消停止）；非 mux 连接/无质量源 → 采样返回 nil 跳过（零回归）。
type QualitySwitchMonitor struct {
	cfg        QualitySwitchConfig
	metrics    qualityMetricsFn
	redialFn   func(ctx context.Context) (*Result, error)
	closeOld   func() error
	lastSwitch time.Time
	badStreak  int
	logger     *slog.Logger
	recorder   qualitySwitchRecorder
}

// NewQualitySwitchMonitor 构造监控器（注入指标采样 + 重拨 + 旧连接关闭）。
// metricsFn 返回 nil 时监控跳过（非 mux 连接零回归）；redialFn 返回 nil 表示不可重拨（跳过切换）。
func NewQualitySwitchMonitor(cfg QualitySwitchConfig, metricsFn qualityMetricsFn) *QualitySwitchMonitor {
	return &QualitySwitchMonitor{
		cfg:     cfg.withDefaults(),
		metrics: metricsFn,
		logger:  slog.Default(),
	}
}

// WithRedial 注入重拨函数（返回新连接 Result；nil = 不可重拨时跳过切换）。
func (m *QualitySwitchMonitor) WithRedial(fn func(ctx context.Context) (*Result, error)) *QualitySwitchMonitor {
	m.redialFn = fn
	return m
}

// WithCloseOld 注入旧连接关闭函数（切换成功后关闭旧连接；nil = 不关闭零回归）。
func (m *QualitySwitchMonitor) WithCloseOld(fn func() error) *QualitySwitchMonitor {
	m.closeOld = fn
	return m
}

// WithLogger 注入日志器（nil = slog.Default）。
func (m *QualitySwitchMonitor) WithLogger(l *slog.Logger) *QualitySwitchMonitor {
	if l != nil {
		m.logger = l
	}
	return m
}

// WithRecorder 注入切换事件回调（metrics 聚合）。
func (m *QualitySwitchMonitor) WithRecorder(rec qualitySwitchRecorder) *QualitySwitchMonitor {
	m.recorder = rec
	return m
}

// degraded 单次采样判定当前连接是否劣化（重传率 > 阈值；无指标 = 不劣化）。
// 返回 true 表示本次采样超阈值——配合连续计数判定整体劣化。
func (m *QualitySwitchMonitor) degraded() bool {
	if m.metrics == nil {
		return false
	}
	mm := m.metrics()
	if mm == nil {
		return false // 无数据（尚未有统计）→ 不判劣化
	}
	sent := mm.FramesSent.Load()
	rt := mm.Retransmits.Load()
	total := sent + rt
	if total <= 0 {
		return false
	}
	rate := float64(rt) / float64(total)
	if rate > 1 {
		rate = 1
	}
	return rate > m.cfg.Threshold
}

// Run 启动周期采样监控（阻塞；ctx 取消停止）。返回 nil（ctx 取消）或切换相关错误。
func (m *QualitySwitchMonitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.checkOnce(ctx)
		}
	}
}

// checkOnce 执行一次采样 + 劣化判定 + 切换。
func (m *QualitySwitchMonitor) checkOnce(ctx context.Context) {
	if m.cfg.LockTransport {
		// 手动锁定：禁用自动切换（安全开关可观测——只记录一次避免刷日志）。
		m.logger.Debug("quality switch locked by config", "lock_transport", true)
		return
	}
	if m.degraded() {
		m.badStreak++
		m.logger.Info("quality degraded detected", "streak", m.badStreak, "samples", m.cfg.Samples)
		if m.badStreak >= m.cfg.Samples {
			m.badStreak = 0 // 触发切换后重置连续计数
			m.trySwitch(ctx)
		}
		return
	}
	m.badStreak = 0 // 健康采样重置
}

// trySwitch 执行一次质量切换（冷却内跳过）。
func (m *QualitySwitchMonitor) trySwitch(ctx context.Context) {
	now := time.Now()
	if !m.lastSwitch.IsZero() && now.Sub(m.lastSwitch) < m.cfg.Cooldown {
		m.logger.Info("quality switch skipped by cooldown", "remaining", m.cfg.Cooldown-now.Sub(m.lastSwitch))
		return
	}
	if m.redialFn == nil {
		m.logger.Warn("quality switch: no redial function, skip")
		return
	}
	m.logger.Info("quality switch: redialing", "threshold", m.cfg.Threshold)
	newRes, err := m.redialFn(ctx)
	if err != nil || newRes == nil {
		m.logger.Warn("quality switch redial failed", "error", err)
		RecordQualitySwitch("redial_failed")
		return
	}
	m.lastSwitch = now
	if m.closeOld != nil {
		if cerr := m.closeOld(); cerr != nil {
			m.logger.Warn("quality switch: close old conn failed", "error", cerr)
		}
	}
	RecordQualitySwitch("degraded")
	if m.recorder != nil {
		m.recorder("degraded", "old", newRes.Kind)
	}
	m.logger.Info("quality switch complete", "new_kind", newRes.Kind)
}

// MuxMetricsOfConn 从建连 Result 提取 mux 指标采样函数（供监控器装配）。
// 非 mux 连接（Result.Conn 非 *MuxStreamConn）→ 返回 nil（监控跳过零回归）。
func MuxMetricsOfConn(conn interface {
	Mux() *mux.Mux
}) qualityMetricsFn {
	if conn == nil {
		return nil
	}
	m := conn.Mux()
	if m == nil {
		return nil
	}
	return m.Metrics
}
