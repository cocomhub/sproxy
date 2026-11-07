// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

// alerts.go 是阈值告警引擎（roadmap P1 阈值告警）：
//
//   - Engine：规则（source + threshold + channels）+ 状态机（FIRING→OK，
//     同 source+key 只发一次通知；恢复自动发恢复通知）+ 事件驱动（卷 degraded /
//     同步失败 / 登录锁定即时告警）+ 定时轮询（磁盘水位）。
//   - 事件源挂点（装配层）：externalVolumeState degraded 命中、syncmgr 任务
//     StatusFailed 转换、loginFailTracker 首次锁定——各调用方注入回调。
//   - 分发复用 notify.NotifyCenter（规则匹配 + 去抖 + 重试 + 历史）。

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/ai"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/server/notify"
)

// SourceNATFailure 是 NAT/中继拨号失败事件源（hub/relay/webrtc STUN/TURN
// 打洞或出口拨号失败；roadmap 11.1-①）。装配层经 OnNATFailure/OnNATRecovered
// 注入，恢复（同 peer 后续拨号成功）自动发恢复通知。
const SourceNATFailure = "nat_failure"

// Engine 是告警引擎。
type Engine struct {
	mu        sync.Mutex
	rules     []config.AlertRule
	state     map[string]string // key=source+object → "firing" / "ok"
	channels  map[string]notify.Notifier
	logger    *slog.Logger
	done      chan struct{}
	wg        sync.WaitGroup
	diskUsage func() (used, cap int64)
	// quotaUsage 是 per-owner 配额水位读取器（quota_watermark source；nil = 不检查）。
	quotaUsage func(owner string) (used, cap int64)
	// ownerList 是配额水位轮询的 owner 列表（装配层注入；nil = 不轮询）。
	ownerList func() []string
	poll      time.Duration
	// advisor 是告警建议器（roadmap 11.9-⑥；SetAdvisor 注入，nil = 现状零回归）。
	advisor adviser
}

// NewEngine 构造告警引擎（logger nil → slog.Default）。
func NewEngine(cfg config.AlertConfig, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	eng := &Engine{
		rules:    cfg.Rules,
		state:    map[string]string{},
		channels: map[string]notify.Notifier{},
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

// ReloadRules 热加载规则集（SIGHUP 软配置路径）：在锁下原子替换（slices.Clone
// 快照，读方 fire/rulesFor/轮询持锁快照，无撕裂），并**保留既有 firing 状态**
// （state 不清空）——规则变更不误发恢复通知，也不重复告警；新增/删除规则后，
// 下轮事件/轮询按新规则判定。
// 语义约束：阈值类 source（disk_watermark/quota_watermark）新规则在下个轮询
// tick 生效（≤ PollInterval 延迟）；事件类 source（volume_degraded/sync_failed/
// login_locked/nat_failure）下个事件即生效。
func (e *Engine) ReloadRules(rules []config.AlertRule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = slices.Clone(rules)
}

// Register 注册渠道（规则 channels 名 → 渠道）。
func (e *Engine) Register(n notify.Notifier) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.channels[n.Name()]; ok {
		return false
	}
	e.channels[n.Name()] = n
	return true
}

// SetDiskUsageReader 注入磁盘水位读取器（测试用；默认读 globalPool）。
func (e *Engine) SetDiskUsageReader(f func() (used, cap int64)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diskUsage = f
}

// SetQuotaUsageReader 注入 per-owner 配额水位读取器（测试用；装配层读 quotaScope）。
func (e *Engine) SetQuotaUsageReader(f func(owner string) (used, cap int64)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaUsage = f
}

// SetOwnerList 注入配额轮询的 owner 列表（装配层；nil = 不轮询配额水位）。
func (e *Engine) SetOwnerList(f func() []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownerList = f
}

// Close 关闭引擎（停止轮询；幂等）。
func (e *Engine) Close() {
	select {
	case <-e.done:
		return
	default:
		close(e.done)
	}
	e.wg.Wait()
}

// Start 启动磁盘水位轮询（goroutine）。
func (e *Engine) Start() {
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
func (e *Engine) checkDiskWatermark(ctx context.Context) {
	e.mu.Lock()
	usageFn := e.diskUsage
	e.mu.Unlock()
	if usageFn == nil {
		return
	}
	used, capVal := usageFn()
	if capVal <= 0 {
		return
	}
	pct := int(used * 100 / capVal)
	e.mu.Lock()
	rules := make([]config.AlertRule, 0, len(e.rules))
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
func (e *Engine) ownerListSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ownerList == nil {
		return nil
	}
	return e.ownerList()
}

// checkQuotaWatermarks 检查 per-owner 配额水位（quota_watermark 规则；
// 超阈值 → 告警，恢复 → 恢复通知；各 owner 独立去抖）。
func (e *Engine) checkQuotaWatermarks(ctx context.Context, owners []string) {
	e.mu.Lock()
	usageFn := e.quotaUsage
	e.mu.Unlock()
	if usageFn == nil {
		return
	}
	e.mu.Lock()
	var rules []config.AlertRule
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
		used, capVal := usageFn(owner)
		if capVal <= 0 {
			continue
		}
		pct := int(used * 100 / capVal)
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
func (e *Engine) OnVolumeDegraded(ctx context.Context, volName, detail string) {
	e.mu.Lock()
	rules := e.rulesFor("volume_degraded")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "volume_degraded\x00"+volName, r, fmt.Sprintf("卷 %s degraded: %s", volName, detail))
	}
}

// OnVolumeRecovered 卷恢复事件。
func (e *Engine) OnVolumeRecovered(ctx context.Context, volName string) {
	e.mu.Lock()
	rules := e.rulesFor("volume_degraded")
	e.mu.Unlock()
	for _, r := range rules {
		e.recover(ctx, "volume_degraded\x00"+volName, r, fmt.Sprintf("卷 %s 已恢复", volName))
	}
}

// OnSyncFailed 同步任务失败事件（syncmgr 装配层回调）。
func (e *Engine) OnSyncFailed(ctx context.Context, taskID, detail string) {
	e.mu.Lock()
	rules := e.rulesFor("sync_failed")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "sync_failed\x00"+taskID, r, fmt.Sprintf("同步任务 %s 失败: %s", taskID, detail))
	}
}

// OnLoginLocked 认证暴力破解锁定事件（loginFailTracker 装配层回调）。
func (e *Engine) OnLoginLocked(ctx context.Context, ak string) {
	e.mu.Lock()
	rules := e.rulesFor("login_locked")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "login_locked\x00"+ak, r, fmt.Sprintf("AK %s 触发失败锁定（暴力破解疑似）", ak))
	}
}

// OnNATFailure NAT/中继拨号失败事件（mesh 装配层回调：cloud 出口拨号 / mesh
// node 出口与 webrtc 打洞失败）。按 peer 独立去抖（key=nat_failure\x00<peer>），
// 同 peer 首次失败通知、重复失败去抖。
func (e *Engine) OnNATFailure(ctx context.Context, peer, detail string) {
	e.mu.Lock()
	rules := e.rulesFor(SourceNATFailure)
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, SourceNATFailure+"\x00"+peer, r, fmt.Sprintf("NAT/中继拨号失败 %s: %s", peer, detail))
	}
}

// OnNATRecovered NAT/中继拨号恢复事件（同 peer 后续拨号成功时调用）。
// recover 内部已判 state != "firing" 即 no-op，故每次拨号成功都可安全调用。
func (e *Engine) OnNATRecovered(ctx context.Context, peer string) {
	e.mu.Lock()
	rules := e.rulesFor(SourceNATFailure)
	e.mu.Unlock()
	for _, r := range rules {
		e.recover(ctx, SourceNATFailure+"\x00"+peer, r, fmt.Sprintf("NAT/中继拨号已恢复 %s", peer))
	}
}

// OnChecksumMismatch 全仓 checksum 巡检发现坏文件/缺失文件事件（roadmap 11.3-⑩）：
// mismatched+missing 数 > 0 → source=checksum_mismatch 规则告警（装配层挂点，nil 引擎零回归）。
// 与既有 volume degraded 模式同构（fire + 状态机去抖；巡检是低频事件，无需 recover 挂点）。
func (e *Engine) OnChecksumMismatch(ctx context.Context, owner string, count int, detail string) {
	e.mu.Lock()
	rules := e.rulesFor("checksum_mismatch")
	e.mu.Unlock()
	for _, r := range rules {
		e.fire(ctx, "checksum_mismatch\x00"+owner, r, fmt.Sprintf("checksum 巡检发现 %d 个不一致文件（owner=%s）: %s", count, owner, detail))
	}
}

// rulesFor 返回匹配 source 的规则（须持锁）。
func (e *Engine) rulesFor(source string) []config.AlertRule {
	var out []config.AlertRule
	for _, r := range e.rules {
		if r.Source == source {
			out = append(out, r)
		}
	}
	return out
}

// fire 触发告警（状态机：首次 firing → 通知；已 firing → 去抖）。
func (e *Engine) fire(ctx context.Context, key string, r config.AlertRule, text string) {
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
func (e *Engine) recover(ctx context.Context, key string, r config.AlertRule, text string) {
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
func (e *Engine) channelsFor(r config.AlertRule) []notify.Notifier {
	var out []notify.Notifier
	for _, name := range r.Channels {
		if ch, ok := e.channels[name]; ok {
			out = append(out, ch)
		}
	}
	return out
}

// dispatch 分发到各渠道（同步；告警低频，避免 goroutine 泄漏/竞态）。
// 组装后同步调用 advisor.Advise（失败返回 "" → 追加为空即固定模板原样，fail-closed）。
func (e *Engine) dispatch(ctx context.Context, channels []notify.Notifier, key, text string) {
	parts := splitKey(key)
	advisor := e.advisorSnapshot()
	advice := ""
	if advisor != nil {
		advice = advisor.Advise(ctx, key, fmt.Sprintf("[sproxy] 告警: %s", parts[0]), text)
	}
	text = ai.AppendAdvice(text, advice)
	for _, ch := range channels {
		msg := notify.NotifyMessage{
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

// NewFromConfig 从 config.AlertConfig 装配告警引擎（未启用 → nil）。
func NewFromConfig(cfg config.AlertConfig, logger *slog.Logger) *Engine {
	if !cfg.Enabled || len(cfg.Rules) == 0 {
		return nil
	}
	return NewEngine(cfg, logger)
}

// AdoptNotifyChannels 把通知中心渠道并入告警引擎（规则 channels 名共用）。
// 遍历 nc.channels（持锁）；渠道是共享实例（告警与通知同渠道）。
func (e *Engine) AdoptNotifyChannels(nc *notify.NotifyCenter) {
	if nc == nil {
		return
	}
	for _, ch := range nc.Channels() {
		e.Register(ch)
	}
}
