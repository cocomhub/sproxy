// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// backend_settings.go 让 secretdata 后端实现 registry 的**卷设置分区**能力
// （设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §5.3、§7）。
//
// 边界（为什么能力实现在 backend 而不是 FS）：`registry.Set.External()` 返回的是**原始
// backend 实例**，装配层只能对句柄做能力断言。把 Settings/UpdateSettings 放在 backend 上
// 既符合既有可选能力的落位（Presigner/MetaPrewarmer 都在 backend），也不需要把 HTTP/持久化
// 概念下推给纯 FS 层。

// settingsSectionBlockPolicy 是 secretdata 暴露的唯一设置分区标识（URL 段）。
const settingsSectionBlockPolicy = "block_policy"

// 编译期断言：secretdata 后端必须提供三个卷设置能力。
var (
	_ registry.SettingsProvider    = (*backend)(nil)
	_ registry.SettingsWriter      = (*backend)(nil)
	_ registry.SettingsStoreSetter = (*backend)(nil)
)

// Settings 实现 registry.SettingsProvider：返回 `block_policy` 分区的 schema 与当前值。
//
// 纯内存读（不 I/O）：值来自 FS 的规则快照；ReadOnly 由「是否已注入持久化后端」决定——
// 未注入时无法落盘，UI 应禁用保存而不是让人以为改了（静默丢失配置比拒绝更糟）。
func (b *backend) Settings(_ context.Context) ([]registry.VolumeSettings, error) {
	runtime, baseline := b.fs.PolicyState()
	value := blockPolicyView{
		Rules:           rulesOf(runtime),
		BaselineRules:   rulesOf(baseline),
		EffectiveSource: effectiveSourceOf(runtime, baseline),
		Limits: blockPolicyLimits{
			HardMax:        policy.HardMaxSingleBytes,
			VolumeBlockMax: b.fs.opts.Block.Max,
			EffectiveMax:   effectiveSingleMax(b.fs.opts.Block.Max),
		},
		Version: runtime.Version(), // nil-safe（Compiled.Version 对 nil 返回 0）
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("secretdata: 序列化分块规则失败: %w", err)
	}
	return []registry.VolumeSettings{{
		Key:         settingsSectionBlockPolicy,
		Label:       "分块规则",
		Description: "按「路径正则 + 体积区间」选择块级分块模式。single = 整文件单块（1 个底层 blob），random = 卷默认变长分块。规则只影响新写入的文件，不会重写已存在的文件。",
		Fields: []registry.FieldSchema{
			{Key: "rules", Label: "规则（按 priority 降序，首命中生效）", Type: "rules"},
			{Key: "max_size_limit", Label: "单条规则块大小上限（字节）", Type: "number"},
		},
		Value:    raw,
		ReadOnly: b.loadSettingsStore() == nil,
	}}, nil
}

// blockPolicyView 是 `block_policy` 分区的**回显形状**（向后兼容：保留 rules/version 字段，
// 新增 baseline_rules / effective_source / limits 供排障与 UI 提示）。
//
// 为什么需要：清空运行期规则后 `rules` 为空，但实际生效的是静态基线或卷默认——只回显运行期
// 规则会让运维无法区分「没配」与「配了但回落到基线」，也无法在 UI 提示单条规则的真实上限。
type blockPolicyView struct {
	Rules           []policy.Rule     `json:"rules"`
	BaselineRules   []policy.Rule     `json:"baseline_rules,omitempty"`
	EffectiveSource string            `json:"effective_source"`
	Limits          blockPolicyLimits `json:"limits"`
	Version         int64             `json:"version"`
}

// blockPolicyLimits 是单条 `single` 规则的真实上限（UI 校验与提示复用）。
type blockPolicyLimits struct {
	// HardMax 是算法层硬顶（shardseal.MaxSingleBlockBytes）。
	HardMax int64 `json:"hard_max"`
	// VolumeBlockMax 是本卷 block_policy.max。
	VolumeBlockMax int64 `json:"volume_block_max"`
	// EffectiveMax = min(HardMax, VolumeBlockMax>0 ? VolumeBlockMax : HardMax)。
	EffectiveMax int64 `json:"effective_max"`
}

// rulesOf 返回快照规则（nil ⇒ nil）。
func rulesOf(c *policy.Compiled) []policy.Rule { return c.Rules() }

// effectiveSourceOf 判定**实际生效**的规则来源（与 FS 的两级优先级一致）。
func effectiveSourceOf(runtime, baseline *policy.Compiled) string {
	switch {
	case runtime != nil && runtime.Version() >= 0 && len(runtime.Rules()) > 0:
		return "runtime"
	case baseline != nil:
		return "baseline"
	default:
		return "volume_default"
	}
}

// effectiveSingleMax 返回单条 single 规则的实际可用上限（卷级 max 未知/未配时取硬顶）。
func effectiveSingleMax(volumeBlockMax int64) int64 {
	if volumeBlockMax > 0 && volumeBlockMax < policy.HardMaxSingleBytes {
		return volumeBlockMax
	}
	return policy.HardMaxSingleBytes
}

// UpdateSettings 实现 registry.SettingsWriter：校验 → 落盘（CAS）→ 热生效。
//
// **顺序不可交换**：先落盘后生效，才能保证「返回成功即已持久化」；反过来会出现「内存生效
// 但重启丢失」。`SetPolicy` 是无失败的 atomic 交换 ⇒ 落盘成功后不存在半生效窗口。
func (b *backend) UpdateSettings(ctx context.Context, section string, payload json.RawMessage) error {
	if section != settingsSectionBlockPolicy {
		return fmt.Errorf("%w: %q（secretdata 仅支持 %q）",
			registry.ErrSettingsSectionUnsupported, section, settingsSectionBlockPolicy)
	}
	store := b.loadSettingsStore()
	if store == nil {
		return fmt.Errorf("%w: 本卷未装配设置持久化后端（需要 state_store）", registry.ErrSettingsPayloadInvalid)
	}
	var p policy.Policy
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("%w: 规则载荷非法: %w", registry.ErrSettingsPayloadInvalid, err)
	}
	if _, err := policy.Compile(p, b.fs.opts.Block.Max); err != nil {
		return fmt.Errorf("%w: %w", registry.ErrSettingsPayloadInvalid, err)
	}
	// CAS 期望版本取自**持久化**（不是内存快照）：多节点部署下另一节点可能已写过，用陈旧
	// 内存版本会要么静默覆盖他人修改、要么永久 409（内存快照不刷新就永远读不到新版本）。
	_, curVer, lerr := store.Load(ctx, b.fs.opts.VolumeName)
	if lerr != nil {
		return fmt.Errorf("读取当前卷设置版本: %w", lerr)
	}
	newVer, err := store.Save(ctx, b.fs.opts.VolumeName, payload, curVer)
	if err != nil {
		return err // ErrSettingsVersionConflict 由装配层映射 409
	}
	// 生效（先落盘后生效）：空规则 = **清空运行期覆盖**，回落静态基线（不是「用一个空快照
	// 永久压制基线」——设计 §7 的优先级链要求可回滚）。
	if len(p.Rules) == 0 {
		b.fs.SetPolicy(nil)
		return nil
	}
	applied := policy.Policy{Rules: p.Rules, Version: newVer}
	compiled, cerr := policy.Compile(applied, b.fs.opts.Block.Max)
	if cerr != nil {
		// 理论不可达（同一 payload 刚校验过），但绝不静默：落盘已成功的版本必须可见。
		return fmt.Errorf("规则已保存但热生效失败（重启后生效）: %w", cerr)
	}
	b.fs.SetPolicy(compiled)
	return nil
}

// SetSettingsStore 实现 registry.SettingsStoreSetter：保存持久化后端并**立即载入一次**。
//
// 失败语义（用户裁定「规则是加速项、非正确性项」）：读失败或存量规则编译失败 ⇒ 告警 +
// 保留当前策略，**返回 nil**（不阻断卷挂载）。否则一个坏规则就能让整卷挂不上。
func (b *backend) SetSettingsStore(store registry.SettingsStore) error {
	b.settingsStore.Store(&store)
	if store == nil {
		return nil
	}
	raw, version, err := store.Load(context.Background(), b.fs.opts.VolumeName)
	if err != nil {
		slog.Warn("secretdata: 载入持久化分块规则失败（保留当前策略）",
			"volume", b.fs.opts.VolumeName, "error", err)
		return nil
	}
	if raw == nil {
		return nil // 无记录 = 用静态基线/默认
	}
	var p policy.Policy
	if uerr := json.Unmarshal(raw, &p); uerr != nil {
		slog.Warn("secretdata: 持久化分块规则载荷损坏（保留当前策略）",
			"volume", b.fs.opts.VolumeName, "error", uerr)
		return nil
	}
	p.Version = version
	if len(p.Rules) == 0 {
		b.fs.SetPolicy(nil) // 清空 = 回落静态基线/卷默认
		return nil
	}
	compiled, cerr := policy.Compile(p, b.fs.opts.Block.Max)
	if cerr != nil {
		slog.Warn("secretdata: 持久化分块规则校验失败（保留当前策略）",
			"volume", b.fs.opts.VolumeName, "error", cerr)
		return nil
	}
	b.fs.SetPolicy(compiled)
	slog.Info("secretdata: 已载入持久化分块规则",
		"volume", b.fs.opts.VolumeName, "rules", len(p.Rules), "version", version)
	return nil
}

// settingsStoreField 是 backend 的持久化后端字段（接口值用 atomic.Pointer 包装 ⇒ 可在
// 装配后被替换，且读侧无锁）。注意 atomic.Pointer[T] 存的是 *T，故读取要解引用。
type settingsStoreField = atomic.Pointer[registry.SettingsStore]

// loadSettingsStore 返回当前持久化后端（nil = 未装配）；解引用 atomic 指针。
func (b *backend) loadSettingsStore() registry.SettingsStore {
	if p := b.settingsStore.Load(); p != nil {
		return *p
	}
	return nil
}
