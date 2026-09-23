// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// alerts.go 是阈值告警引擎（roadmap P1 阈值告警）：
//
//   - AlertEngine：规则（source + threshold + channels）+ 状态机（FIRING→OK，
//     同 source+key 只发一次通知；恢复自动发恢复通知）+ 事件驱动（卷 degraded /
//     同步失败 / 登录锁定即时告警）+ 定时轮询（磁盘水位）。
//   - 事件源挂点（装配层）：externalVolumeState degraded 命中、syncmgr 任务
//     StatusFailed 转换、loginFailTracker 首次锁定——各调用方注入回调。
//   - 分发复用 NotifyCenter（规则匹配 + 去抖 + 重试 + 历史）。

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AlertRule 是告警规则（source 匹配 + threshold + channels）。
type AlertRule struct {
	Source    string   `yaml:"source" mapstructure:"source"`       // disk_watermark / volume_degraded / sync_failed / login_locked
	Threshold int      `yaml:"threshold" mapstructure:"threshold"` // 百分比（disk_watermark 用）
	Channels  []string `yaml:"channels" mapstructure:"channels"`
}

// AlertConfig 是告警引擎配置（config.go 引用）。
type AlertConfig struct {
	Enabled bool        `yaml:"enabled" mapstructure:"enabled"`
	Rules   []AlertRule `yaml:"rules" mapstructure:"rules"`
	// PollInterval 是磁盘水位轮询间隔（默认 60s；0 = 关闭轮询只走事件驱动）。
	PollInterval time.Duration `yaml:"poll_interval" mapstructure:"poll_interval"`
}

// AlertEngine 是告警引擎。
type AlertEngine struct {
	mu        sync.Mutex
	rules     []AlertRule
	state     map[string]string // key=source+object → "firing" / "ok"
	channels  map[string]Notifier
	logger    *slog.Logger
	done      chan struct{}
	wg        sync.WaitGroup
	diskUsage func() (used, cap int64)
	// quotaUsage 是 per-owner 配额水位读取器（quota_watermark source；nil = 不检查）。
	quotaUsage func(owner string) (used, cap int64)
	// ownerList 是配额水位轮询的 owner 列表（装配层注入；nil = 不轮询）。
	ownerList func() []string
	poll      time.Duration
}

// NewAlertEngine 构造告警引擎（logger nil → slog.Default）。
func NewAlertEngine(cfg AlertConfig, logger *slog.Logger) *AlertEngine {
	if logger == nil {
		logger = slog.Default()
	}
	eng := &AlertEngine{
		rules:    cfg.Rules,
		state:    map[string]string{},
		channels: map[string]Notifier{},
		logger:   logger,
		done:     make(chan struct{}),
	}
	if cfg.PollInterval <= 0 {
		eng.poll = 60 * time.Second
	} else {
		eng.poll = cfg.PollInterval
	}
	return eng
}

// Register 注册渠道（规则 channels 名 → 渠道）。
func (e *AlertEngine) Register(n Notifier) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.channels[n.Name()]; ok {
		return false
	}
	e.channels[n.Name()] = n
	return true
}

// SetDiskUsageReader 注入磁盘水位读取器（测试用；默认读 globalPool）。
func (e *AlertEngine) SetDiskUsageReader(f func() (used, cap int64)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diskUsage = f
}

// SetQuotaUsageReader 注入 per-owner 配额水位读取器（测试用；装配层读 quotaScope）。
func (e *AlertEngine) SetQuotaUsageReader(f func(owner string) (used, cap int64)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaUsage = f
}

// SetOwnerList 注入配额轮询的 owner 列表（装配层；nil = 不轮询配额水位）。
func (e *AlertEngine) SetOwnerList(f func() []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownerList = f
}

// Close 关闭引擎（停止轮询；幂等）。
func (e *AlertEngine) Close() {
	select {
	case <-e.done:
		return
	default:
		close(e.done)
	}
	e.wg.Wait()
}

// Start 启动磁盘水位轮询（goroutine）。
func (e *AlertEngine) Start() {
	if e.poll <= 0 {
		return
	}
	e.wg.Go(func() {
		t := time.NewTicker(e.poll)
		defer t.Stop()
		for {
			select {
			case <-e.done:
				return
			case <-t.C:
				e.checkDiskWatermark(context.Background())
				e.checkQuotaWatermarks(context.Background(), e.ownerListSnapshot())
			}
		}
	})
}

// checkDiskWatermark 检查磁盘水位（disk_watermark 规则；超阈值 → 告警，恢复 → 恢复通知）。
func (e *AlertEngine) checkDiskWatermark(ctx context.Context) {
	e.mu.Lock()
	usageFn := e.diskUsage
	e.mu.Unlock()
	if usageFn == nil {
		return
	}
	used, cap := usageFn()
	if cap <= 0 {
		return
	}
	pct := int(used * 100 / cap)
	e.mu.Lock()
	rules := make([]AlertRule, 0, len(e.rules))
	for _, r := range e.rules {
		if r.Source == "disk_watermark" {
			rules = append(rules, r)
		}
	}
	e.mu.Unlock()
	for _, r := range rules {
		key := "disk_watermark\x00" + "storage"
		if pct >= r.Threshold {
			e.fire(ctx, key, r, fmt.Sprintf("磁盘水位 %d%% ≥ 阈值 %d%%", pct, r.Threshold))
		} else {
			e.recover(ctx, key, r, fmt.Sprintf("磁盘水位已恢复至 %d%%", pct))
		}
	}
}

// ownerListSnapshot 返回配额轮询 owner 列表（无锁副本）。
func (e *AlertEngine) ownerListSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ownerList == nil {
		return nil
	}
	return e.ownerList()
}

// checkQuotaWatermarks 检查 per-owner 配额水位（quota_watermark 规则；
// 超阈值 → 告警，恢复 → 恢复通知；各 owner 独立去抖）。
func (e *AlertEngine) checkQuotaWatermarks(ctx context.Context, owners []string) {
	e.mu.Lock()
	usageFn := e.quotaUsage
	e.mu.Unlock()
	if usageFn == nil {
		return
	}
	e.mu.Lock()
	var rules []AlertRule
	for _, r := range e.rules {
		if r.Source == "quota_watermark" {
			rules = append(rules, r)
		}
	}
	e.mu.Unlock()
	if len(rules) == 0 {
		return
	}
	for _, owner := range owners {
		used, cap := usageFn(owner)
		if cap <= 0 {
			continue
		}
		pct := int(used * 100 / cap)
		key := "quota_watermark" + string([]byte{0}) + owner
		for _, r := range rules {
			if pct >= r.Threshold {
				e.fire(ctx, key, r, fmt.Sprintf("配额 %s 水位 %d%% ≥ 阈值 %d%%", owner, pct, r.Threshold))
			} else {
				e.recover(ctx, key, r, fmt.Sprintf("配额 %s 已恢复至 %d%%", owner, pct))
			}
		}
	}
}

// OnVolumeDegraded 卷 degraded 事件（外部卷探针失败）。
func (e *AlertEngine) OnVolumeDegraded(ctx context.Context, volName, detail string) {
	e.mu.Lock()
	rules := e.rulesFor("volume_degraded")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "volume_degraded\x00"+volName, r, fmt.Sprintf("卷 %s degraded: %s", volName, detail))
	}
}

// OnVolumeRecovered 卷恢复事件。
func (e *AlertEngine) OnVolumeRecovered(ctx context.Context, volName string) {
	e.mu.Lock()
	rules := e.rulesFor("volume_degraded")
	e.mu.Unlock()
	for _, r := range rules {
		e.recover(ctx, "volume_degraded\x00"+volName, r, fmt.Sprintf("卷 %s 已恢复", volName))
	}
}

// OnSyncFailed 同步任务失败事件（syncmgr 装配层回调）。
func (e *AlertEngine) OnSyncFailed(ctx context.Context, taskID, detail string) {
	e.mu.Lock()
	rules := e.rulesFor("sync_failed")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "sync_failed\x00"+taskID, r, fmt.Sprintf("同步任务 %s 失败: %s", taskID, detail))
	}
}

// OnLoginLocked 认证暴力破解锁定事件（loginFailTracker 装配层回调）。
func (e *AlertEngine) OnLoginLocked(ctx context.Context, ak string) {
	e.mu.Lock()
	rules := e.rulesFor("login_locked")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "login_locked\x00"+ak, r, fmt.Sprintf("AK %s 触发失败锁定（暴力破解疑似）", ak))
	}
}

// rulesFor 返回匹配 source 的规则（须持锁）。
func (e *AlertEngine) rulesFor(source string) []AlertRule {
	var out []AlertRule
	for _, r := range e.rules {
		if r.Source == source {
			out = append(out, r)
		}
	}
	return out
}

// fire 触发告警（状态机：首次 firing → 通知；已 firing → 去抖）。
func (e *AlertEngine) fire(ctx context.Context, key string, r AlertRule, text string) {
	e.mu.Lock()
	if e.state[key] == "firing" {
		e.mu.Unlock()
		return
	}
	e.state[key] = "firing"
	channels := e.channelsFor(r)
	e.mu.Unlock()
	e.dispatch(ctx, channels, key, text)
}

// recover 恢复（OK 状态 → 发恢复通知）。
func (e *AlertEngine) recover(ctx context.Context, key string, r AlertRule, text string) {
	e.mu.Lock()
	if e.state[key] != "firing" {
		e.mu.Unlock()
		return
	}
	e.state[key] = "ok"
	channels := e.channelsFor(r)
	e.mu.Unlock()
	e.dispatch(ctx, channels, key, text)
}

// channelsFor 返回规则命中的已注册渠道（**不持锁**；调用方在 e.mu 保护下调用）。
func (e *AlertEngine) channelsFor(r AlertRule) []Notifier {
	var out []Notifier
	for _, name := range r.Channels {
		if ch, ok := e.channels[name]; ok {
			out = append(out, ch)
		}
	}
	return out
}

// dispatch 分发到各渠道（同步；告警低频，避免 goroutine 泄漏/竞态）。
func (e *AlertEngine) dispatch(ctx context.Context, channels []Notifier, key, text string) {
	parts := splitKey(key)
	for _, ch := range channels {
		msg := NotifyMessage{
			Title:  fmt.Sprintf("[sproxy] 告警: %s", parts[0]),
			Text:   text,
			Object: parts[1],
			Action: parts[0],
		}
		if err := ch.Send(ctx, msg); err != nil {
			e.logger.Warn("告警发送失败", "channel", ch.Name(), "key", key, "error", err.Error())
		}
	}
}

// splitKey 拆分 "source\x00object"。
func splitKey(key string) []string {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return []string{key[:i], key[i+1:]}
		}
	}
	return []string{key, ""}
}

// 避免 time 未用（Start 用 time.Ticker）。
var _ = time.Second

// newAlertEngineFromConfig 从 AlertConfig 装配告警引擎（未启用 → nil）。
func newAlertEngineFromConfig(cfg AlertConfig, logger *slog.Logger) *AlertEngine {
	if !cfg.Enabled || len(cfg.Rules) == 0 {
		return nil
	}
	return NewAlertEngine(cfg, logger)
}

// AdoptNotifyChannels 把通知中心渠道并入告警引擎（规则 channels 名共用）。
// 遍历 nc.channels（持锁）；渠道是共享实例（告警与通知同渠道）。
func (e *AlertEngine) AdoptNotifyChannels(nc *NotifyCenter) {
	if nc == nil {
		return
	}
	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, ch := range nc.channels {
		e.Register(ch)
	}
}
