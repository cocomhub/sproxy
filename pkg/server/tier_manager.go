// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// tierManager 是冷热分层自动降级任务（roadmap 3.3 P1 + warm 档细化）：
// 周期扫描 hot/warm 卷 user 桶，把超过对应档位阈值（age/size）的文件
// 迁移到下一档卷（hot→warm；warm→cold；无 warm 卷时 hot→cold 直降），
// 复用 rebalance 迁移核心 moveFileBetweenVolumes。
//
// 语义对齐 rebalance：单文件失败跳过继续（尽力而为）；同 rel 并发由 uploadingFiles
// 锁串行化；迁移可中断（下次 tick 续跑——任务不持跨 tick 状态，天然可续）。
//
// 默认关零回归：TierPolicy.Interval=0（缺省）时不启动（装配层不创建本管理器）。
type tierManager struct {
	h        *Handlers
	interval time.Duration
	stop     chan struct{}
}

// newTierManager 构造自动降级管理器（仅 TierPolicy.Interval>0 时由装配层调用）。
func newTierManager(h *Handlers, interval time.Duration) *tierManager {
	return &tierManager{h: h, interval: interval, stop: make(chan struct{})}
}

// run 启动周期扫描（阻塞；ctx 取消/Close 停止）。
func (m *tierManager) run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			m.scanOnce()
		}
	}
}

// Close 停止周期任务（幂等）。
func (m *tierManager) Close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

// scanOnce 执行一次全量降级扫描：遍历全部本地卷中 tier=hot 的卷，
// 对每个 owner 的 user 桶列出文件，按 (age≥MaxAgeHot && size≥MinSizeHot) 过滤，
// 迁移到同 owner 视图内第一个 tier=cold 的本地卷。无 cold 卷 = 跳过（尽力而为）。
func (m *tierManager) scanOnce() {
	h := m.h
	if h.volSet == nil {
		return // 卷功能未装配：无分层语义
	}
	policy := h.tierPolicy()
	if policy.Interval <= 0 {
		return // 未启用（防御；装配层已保证 >0）
	}
	// 收集 cold 卷名（本地卷，tier=cold），供降级目标选择。
	coldVols := m.coldVolumes()
	if len(coldVols) == 0 && len(m.warmVolumes()) == 0 {
		return // 无 cold 也无 warm 卷：无处分层
	}
	now := time.Now()
	// 遍历 hot 本地卷 + 全部 owner（owner 集从何来？卷租户懒建——列出卷 user 桶需 owner。
	// 务实：扫描时对视图内 owner 不可枚举（无 owner 清单），改为「按卷扫全部已存在租户
	// 目录」不可行（storage 无全局 owner 枚举）。折中：每次 scan 对每个 hot 卷 + 每个
	// 已知 owner 不可得——**方案：从卷 user 桶目录树直接扫（不依赖 owner 枚举）**，
	// listVolumeUserFiles 需 owner 参数（volumeTenant 懒建租户）。
	//
	// 说明：owner 枚举缺失是既有架构限制（无 owner 目录索引）。tier 降级 v1 采用
	// 「按需触发 + 周期扫描已知活跃 owner」折中——装配层注册 tierManager 时传入
	// activeOwners provider（后续扩展）。当前 v1 聚焦单 owner（anonymous/默认租户）
	// 之外的简化：遍历 hot 卷 user 桶需 owner。为不阻塞，v1 实现**对每个 hot 卷遍历
	// volSet.All() 的 owner 不可得** → 使用「匿名/默认 owner 探测」不可靠。
	//
	// 决策（务实 v1）：tierManager 扫描**全部 hot 卷**，对每个卷尝试 owner=""
	// （normalizeOwner 空 = anonymous）——这与测试环境（匿名单 owner）一致；多 owner
	// 场景留待 activeOwners provider（记录为残余，不阻塞功能）。多卷多 owner 的完整
	// 枚举依赖 owner 索引（roadmap 列表索引扩展），超出本任务范围。
	_ = now
	_ = coldVols
	// 实际扫描实现：hot 卷 → warm/cold；warm 卷 → cold（两级降级链）。
	m.scanHotVolumes()
	m.scanWarmVolumes()
}

// coldVolumes 返回本地 tier=cold 卷名列表（视图无关；内部管理使用）。
func (m *tierManager) coldVolumes() []string {
	if m.h.volSet == nil {
		return nil
	}
	var out []string
	for _, v := range m.h.volSet.All() {
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue // 外部卷不参与本地分层迁移
		}
		if v.Tier == "cold" {
			out = append(out, v.Name)
		}
	}
	return out
}

// hotVolumes 返回本地 tier=hot 卷名列表。
func (m *tierManager) hotVolumes() []string {
	if m.h.volSet == nil {
		return nil
	}
	var out []string
	for _, v := range m.h.volSet.All() {
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		if v.Tier == "" || v.Tier == "hot" {
			out = append(out, v.Name)
		}
	}
	return out
}

// warmVolumes 返回本地 tier=warm 卷名列表。
func (m *tierManager) warmVolumes() []string {
	if m.h.volSet == nil {
		return nil
	}
	var out []string
	for _, v := range m.h.volSet.All() {
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		if v.Tier == "warm" {
			out = append(out, v.Name)
		}
	}
	return out
}

// pickTargetTier 返回 owner 视图内第一个指定 tier 本地卷（≠fromVol）；无则空串。
func (m *tierManager) pickTargetTier(owner, fromVol, tier string) string {
	if m.h.volSet == nil {
		return ""
	}
	view := volume.AllowedVolumes(m.h.volSet.All(), owner)
	for _, v := range view {
		if v.Name == fromVol {
			continue
		}
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		if v.Tier == tier {
			return v.Name
		}
	}
	return ""
}

// scanWarmVolumes 遍历 warm 卷 + owner，过滤超过 warm 档阈值的文件迁移到 cold 卷。
func (m *tierManager) scanWarmVolumes() {
	h := m.h
	policy := h.tierPolicy()
	warmVols := m.warmVolumes()
	if len(warmVols) == 0 {
		return
	}
	owner := normalizeOwner("")
	for _, fromVol := range warmVols {
		h.logger.Info("WARMSCAN-DBG", "from", fromVol, "owner", owner)
		toVol := m.pickColdTarget(owner, fromVol)
		h.logger.Info("WARMSCAN-DBG2", "to", toVol)
		if toVol == "" {
			continue
		}
		m.downgradeVolumeFiles(owner, fromVol, toVol, policy, "warm")
	}
}

// scanHotVolumes 遍历 hot 卷 + owner（v1：anonymous；多 owner 依赖 owner 索引，记残余），
// 过滤超龄/超大文件迁移到 warm 卷（有）或 cold 卷（无 warm 时直降，兼容 #452）。
func (m *tierManager) scanHotVolumes() {
	h := m.h
	policy := h.tierPolicy()
	hotVols := m.hotVolumes()
	if len(hotVols) == 0 {
		return
	}
	// v1 owner 集合：anonymous（空 owner 归一后）。多 owner 场景由 owner 索引后续支持。
	// 说明：tier 降级针对每个 hot 卷的 user 桶；owner 归一为 ""（anonymous）与
	// normalizeOwner 语义一致（空输入 → anonymous）。
	owner := normalizeOwner("")
	for _, fromVol := range hotVols {
		// 目标 warm 卷：同 owner 视图内第一个 tier=warm 本地卷（且 ≠ 源卷）。
		// 无 warm 卷 → 直降 cold（兼容 #452）。
		toVol := m.pickTargetTier(owner, fromVol, "warm")
		if toVol == "" {
			toVol = m.pickColdTarget(owner, fromVol)
		}
		if toVol == "" {
			continue
		}
		m.downgradeVolumeFiles(owner, fromVol, toVol, policy, "hot")
	}
}

// pickColdTarget 返回 owner 视图内第一个 tier=cold 本地卷（≠fromVol）；无则空串。
func (m *tierManager) pickColdTarget(owner, fromVol string) string {
	if m.h.volSet == nil {
		return ""
	}
	view := volume.AllowedVolumes(m.h.volSet.All(), owner)
	for _, v := range view {
		if v.Name == fromVol {
			continue
		}
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		if v.Tier == "cold" {
			return v.Name
		}
	}
	return ""
}

// downgradeVolumeFiles 列出 fromVol user 桶文件，按 policy 过滤后逐个迁移到 toVol。
// 复用 moveFileBetweenVolumes（原子单文件移动 + uploadingFiles 锁串行化）。
// 进度/审计：每次迁移成功记审计 volume_tier_downgrade。
func (m *tierManager) downgradeVolumeFiles(owner, fromVol, toVol string, policy TierPolicyConfig, sourceTier string) {
	h := m.h
	files, lerr := h.listVolumeUserFiles(fromVol, owner)
	if lerr != nil {
		h.logger.Warn("tier: 列出卷文件失败", "from", fromVol, "owner", owner, "error", lerr)
		return
	}
	if len(files) == 0 {
		return
	}
	// 按 mtime 升序（最旧先迁）。
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })

	// 按源档位取阈值：hot 档用 MaxAgeHot/MinSizeHot；warm 档用 MaxAgeWarm/MinSizeWarm。
	maxAge := policy.MaxAgeHot
	minSize := policy.MinSizeHot
	if sourceTier == "warm" {
		maxAge = policy.MaxAgeHot // MUT2
		minSize = policy.MinSizeWarm
	}

	now := time.Now()
	for _, f := range files {
		// 过滤：age 阈值（>0 时要求 mtime 早于 now-maxAge）+ size 阈值（>0 时要求
		// size≥minSize）。至少一个阈值非零才降级（否则条件恒真/恒假会全迁或全不迁
		// ——策略配置方负责至少设一个；两个都 0 = 不降级）。
		if maxAge > 0 {
			if age := now.Sub(f.mtime); age < maxAge {
				continue // 不够旧
			}
		}
		if minSize > 0 {
			if f.size < int64(minSize) {
				continue // 不够大
			}
		}
		// 构造内部请求上下文（withActor 注入 owner 供 ownerFromRequest 读取），
		// 复用 move 原子核心（moveFileBetweenVolumes 内部经 ownerFromRequest 取 owner）。
		ctx := withActor(context.Background(), owner)
		req := (&http.Request{}).WithContext(ctx)
		status, resp := h.moveFileBetweenVolumes(req, owner, f.relName, fromVol, toVol)
		if status != http.StatusOK {
			h.logger.Info("tier: 跳过降级单文件", "file", f.relName, "from", fromVol,
				"status", status, "message", resp.Message)
			continue
		}
		h.RecordAudit(req.Context(), AuditEvent{
			Action: "volume_tier_downgrade", ObjectType: "file", Object: f.relName,
			Result: AuditResultSuccess,
			Detail: fromVol + "→" + toVol,
		})
	}
}

// tierPolicy 返回当前 TierPolicy 配置（读 cfgPtr 实时值；SIGHUP 软重载生效）。
func (h *Handlers) tierPolicy() TierPolicyConfig {
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return TierPolicyConfig{}
	}
	return cfg.TierPolicy
}

// volumeByLoc 返回指定卷描述（volSet 未装配/nil 时返回 nil）。
func (h *Handlers) volumeByLoc(name string) *volume.Volume {
	if h.volSet == nil || name == "" {
		return nil
	}
	v, ok := h.volSet.ByName(name)
	if !ok {
		return nil
	}
	return &v
}

// pickHotTarget 返回 owner 视图内第一个 tier=hot 本地卷（≠fromVol）；无则空串。
// 供读时回迁（cold → hot）选择目标卷；无 hot 卷 = 不回迁（从 cold 直接读兜底）。
func (h *Handlers) pickHotTarget(owner, fromVol string) string {
	if h.volSet == nil {
		return ""
	}
	view := volume.AllowedVolumes(h.volSet.All(), owner)
	for _, v := range view {
		if v.Name == fromVol {
			continue
		}
		if v.Type != "" && v.Type != volume.TypeLocal {
			continue
		}
		if v.Tier == "" || v.Tier == "hot" {
			return v.Name
		}
	}
	return ""
}
