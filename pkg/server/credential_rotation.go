// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credential_rotation.go 是**凭据定期自动轮换调度器**：按配置周期检查各 AK 的最旧 SK
// 到期时间，到期前自动 renew（复用 renewCredential 逻辑），并按 keep_old 裁剪多余旧 SK。
//
// 设计（2026-09-17 用户决策）：
//   - credentials.rotation.interval > 0 时启用；0 = 关闭（默认，零回归）。
//   - 每轮（rotationPass）：遍历 Ring 各 AK，最旧 alive 条目的 ExpiresAt ≤ now+NotifyBefore
//     即触发 renew（新 SK 立即生效，旧 SK 宽限期内仍可用）；随后按 keep_old 裁剪：
//     alive 条目按 CreatedAt 降序保留前 keep_old 个，其余真删（DeleteKey）。
//   - 幂等：同一 AK 一轮只轮换一次（到期判断在遍历前完成，renew 在判断后）。
//   - 时钟可注入（now func() time.Time），测试用假时钟驱动到期判断（R14 禁 time.Sleep）。
//   - 调度器 goroutine 与 versionGCLoop 同构（ticker + stop channel + WaitGroup），
//     由 Close() 关闭 rotationStop 停止。

package server

import (
	"sort"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// rotationConfig 是凭据轮换调度配置（从 cfg 拷贝，避免热更新竞态）。
type rotationConfig struct {
	interval     time.Duration
	notifyBefore time.Duration
	keepOld      int
}

// rotationConfigFromCfg 从 Config 提取轮换配置（cfg 为 nil 时返回全零 = 关闭）。
func rotationConfigFromCfg(cfg *Config) rotationConfig {
	if cfg == nil {
		return rotationConfig{}
	}
	return rotationConfig{
		interval:     cfg.Credentials.Rotation.Interval,
		notifyBefore: cfg.Credentials.Rotation.NotifyBefore,
		keepOld:      cfg.Credentials.Rotation.KeepOld,
	}
}

// credentialRotationLoop 按 interval 周期执行一轮凭据轮换检查。
// 作为 goroutine 在 RegisterRoutes 中按配置（interval > 0）启动；由 Close() 通过关闭
// rotationStop 停止。与 versionGCLoop 同构（ticker + stop channel + WaitGroup）。
func (h *Handlers) credentialRotationLoop() {
	ticker := time.NewTicker(h.cfgPtr.Load().Credentials.Rotation.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.rotationStop:
			return
		case <-ticker.C:
			h.rotationPass(rotationConfigFromCfg(h.cfgPtr.Load()), nil)
		}
	}
}

// rotationPass 执行一轮凭据轮换检查。
//
// now 可注入（nil = time.Now，测试注入假时钟驱动到期判断）。
// 对 Ring 中每个 AK：
//  1. 到期判断：最旧 alive 条目（按 CreatedAt 升序的第一个）ExpiresAt 非零 且
//     ≤ now+notifyBefore → 调 renewCredential（entryID 空 → 回退 CoreEntry 作 wrap key；
//     remoteAddr 记 "rotation" 审计来源）。renew 已含持久化。
//  2. keep_old 裁剪：alive 条目按 CreatedAt 降序，保留前 keep_old 个，其余 DeleteKey
//     （真删，非仅过期——keep_old 语义是收缩 SK 表，防条目只增不减）。
//
// 幂等：每个 AK 最多 renew 一次（判断先行，renew 在判断后只执行一次）；失败（renew
// 错误 / ring 未装配）记 Error 日志继续其它 AK（尽力而为，不阻断轮换）。
func (h *Handlers) rotationPass(cfg rotationConfig, now func() time.Time) {
	if cfg.interval <= 0 {
		return // 关闭（interval=0）
	}
	if now == nil {
		now = time.Now
	}
	if h.credentialRing == nil {
		h.logger.Warn("凭据轮换跳过：凭据 Ring 未装配")
		return
	}
	n := now()
	keys := h.credentialRing.Snapshot()
	for i := range keys {
		k := &keys[i]
		alive := aliveEntries(k.Entries, n)
		if len(alive) == 0 {
			continue // 无存活条目（可能已过期/禁用），不轮换
		}
		// 最旧 alive 条目（CreatedAt 最早）。
		oldest := alive[0]
		for _, e := range alive[1:] {
			if e.CreatedAt.Before(oldest.CreatedAt) {
				oldest = e
			}
		}
		// 到期判断：ExpiresAt 非零且 ≤ now+notifyBefore → 轮换。
		if !oldest.ExpiresAt.IsZero() && !oldest.ExpiresAt.After(n.Add(cfg.notifyBefore)) {
			if _, err := h.renewCredential(k.AK, accesskey.ParseMesh(k.AK), "", "rotation"); err != nil {
				h.logger.Warn("凭据轮换 renew 失败", "ak", k.AK, "error", err)
			} else {
				h.logger.Info("凭据自动轮换完成", "ak", k.AK, "oldest_expires", oldest.ExpiresAt.Format(time.RFC3339))
			}
		}
		// keep_old 裁剪（renew 后存活条目数增加，重新取快照）。
		if cfg.keepOld > 0 {
			h.pruneOldKeys(k.AK, cfg.keepOld, n)
		}
	}
}

// aliveEntries 返回条目中存活（alive）子集的深拷贝（与 accesskey.aliveLocked 同判据）。
func aliveEntries(entries []accesskey.SKEntry, now time.Time) []accesskey.SKEntry {
	out := make([]accesskey.SKEntry, 0, len(entries))
	for _, e := range entries {
		if e.Status == accesskey.StatusDisabled {
			continue
		}
		if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// pruneOldKeys 按 keep_old 裁剪：alive 条目按 CreatedAt 降序保留前 keep_old 个，其余
// 真删（DeleteKey）。裁剪目标取快照实时计算（renew 后条目变化可见）。
func (h *Handlers) pruneOldKeys(ak string, keepOld int, now time.Time) {
	key, ok := h.credentialRing.GetKey(ak)
	if !ok {
		return
	}
	alive := aliveEntries(key.Entries, now)
	if len(alive) <= keepOld {
		return
	}
	// 按 CreatedAt 降序（最新在前）。
	sort.SliceStable(alive, func(i, j int) bool { return alive[i].CreatedAt.After(alive[j].CreatedAt) })
	for _, e := range alive[keepOld:] {
		if err := h.credentialRing.DeleteKey(ak, e.ID); err != nil {
			h.logger.Warn("凭据轮换裁剪失败", "ak", ak, "sk_id", e.ID, "error", err)
			continue
		}
		h.logger.Info("凭据轮换裁剪旧 SK", "ak", ak, "sk_id", e.ID)
	}
	if err := h.persistCredentials(); err != nil {
		h.logger.Warn("凭据轮换裁剪后持久化失败", "ak", ak, "error", err)
	}
}
