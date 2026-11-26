// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"encoding/json"
	"errors"
)

// 本文件定义**卷设置分区**的通用能力（设计 docs/designs/2026-11-26-secretdata-block-policy-rules.md §5.3）。
//
// 动机：各卷型（secretdata 为首个）需要在卷详情里暴露可读/可写的配置分区。把能力做成
// 「分区列表 + 字段 schema + 当前值」的通用容器 ⇒ 后续卷型新增配置只需实现
// SettingsProvider/SettingsWriter 并声明字段，**不改路由、不改前端框架**。
//
// 与 Presigner/MetaPrewarmer/IndexRefresher 同模式：本包（registry 通用层）**不提及任何
// 具体后端类型**，装配层用类型断言检测能力，不解包后端实例。
//
// **为什么不做「包装 backend」**：Set.External() 返回的是原始 backend 实例；包一层薄壳会让
// Presigner/URLResolver 等既有能力断言全部落空（静默降级）。故持久化抽象（SettingsStore）
// 由各卷型后端**自己组合**，本包只提供唯一实现。

// VolumeSettings 是一个卷的**设置分区**（可读/可写配置项的通用描述）。
type VolumeSettings struct {
	// Key 是分区标识（URL 路径段；如 "block_policy"）。
	Key string `json:"key"`
	// Label 是 UI 显示名（如 "分块规则"）。
	Label string `json:"label"`
	// Description 是可选的说明文案（UI 提示用）。
	Description string `json:"description,omitempty"`
	// Fields 是分区内的字段 schema（复用建卷表单的 FieldSchema；"rules" 类型表示有序规则表）。
	Fields []FieldSchema `json:"fields"`
	// Value 是当前值（后端自有 JSON 形状，前端按 Fields 渲染）。
	Value json.RawMessage `json:"value,omitempty"`
	// ReadOnly 标记该卷此分区不可写（如后端未装配持久化后端，或该卷型只支持声明式配置）。
	ReadOnly bool `json:"read_only,omitempty"`
}

// SettingsProvider 是可选能力：卷提供可读的设置分区。
// 未实现 ⇒ 调用方按「无可调配置」处理（详情返回空列表，不报错）。
type SettingsProvider interface {
	// Settings 返回全部分区的 schema 与当前值。纯内存读，不应做 I/O。
	Settings(ctx context.Context) ([]VolumeSettings, error)
}

// SettingsWriter 是可选能力：按分区写入设置（payload 由后端自行解析并校验）。
// 不支持的分区/字段 ⇒ 必须返回明确错误（调用方映射 405/400），不得静默忽略。
type SettingsWriter interface {
	UpdateSettings(ctx context.Context, section string, payload json.RawMessage) error
}

// SettingsStore 是「卷设置分区」的**持久化抽象**（装配层注入后端；nil = 仅内存易失）。
type SettingsStore interface {
	// Load 返回该卷当前值 + 版本；无记录 ⇒ (nil, 0, nil)。
	Load(ctx context.Context, volume string) (json.RawMessage, int64, error)
	// Save 以 CAS 写入（expectedVersion 不符 ⇒ ErrSettingsVersionConflict），返回新版本。
	// expectedVersion == 0 表示「期望当前无记录」（create-only）。
	Save(ctx context.Context, volume string, payload json.RawMessage, expectedVersion int64) (int64, error)
	// Delete 清除该卷的持久化设置（幂等）。
	Delete(ctx context.Context, volume string) error
}

// SettingsReloader 是可选能力：从持久化后端**重新载入**卷设置并返回错误。
//
// 与 SettingsStoreSetter 的区别（为什么需要两个）：Setter 在**装配期**调用，失败只能告警并
// 保留当前策略（否则一个坏配置/后端故障就让整卷挂不上）；而运维 refresh 触发的「重载」必须
// **失败可见**——否则多实例收敛实际失败时，运维仍收到成功回执（审查 Important）。
type SettingsReloader interface {
	ReloadSettings(ctx context.Context) error
}

// SettingsStoreSetter 是可选能力：装配层在 StateStore 就绪后把持久化后端注入卷后端。
//
// 为什么需要注入点而不是构造参数：卷后端在 RegisterRoutes → assembleVolumes 期间构造，
// 而 StateStore 在 RegisterRoutes **之后**才装配（cmd/sproxy/root.go）。实现者应保存 store
// 并立即载入一次（读失败仅告警、保留当前策略，不阻断挂载）。
type SettingsStoreSetter interface {
	SetSettingsStore(store SettingsStore) error
}

// 卷设置相关的哨兵错误（装配层据此映射 HTTP 状态码）。
var (
	// ErrSettingsVersionConflict 是 CAS 冲突（调用方映射 409，提示重读）。
	ErrSettingsVersionConflict = errors.New("registry: 卷设置版本冲突")
	// ErrSettingsSectionUnsupported 是该卷不支持请求的分区（映射 405）。
	ErrSettingsSectionUnsupported = errors.New("registry: 卷设置分区不支持")
	// ErrSettingsReloadNotImplemented 是「该后端没有严格重载语义」的**专用**哨兵。
	//
	// 为什么不用通用 syncpkg.ErrUnsupported：inner 的真实失败若恰好 wrap 了 ErrUnsupported，
	// 会被误判成「不支持」而落回吞错路径 ⇒ 真实失败被掩盖（安全审查 2026-11-27）。
	ErrSettingsReloadNotImplemented = errors.New("registry: 后端未实现设置重载")
	// ErrSettingsPayloadInvalid 是请求载荷非法（JSON 损坏/字段越界等**客户端输入**问题，映射 400）。
	// 后端必须用它包住可识别的输入错误——否则装配层只能按字符串前缀猜，猜不中就变成 500。
	ErrSettingsPayloadInvalid = errors.New("registry: 卷设置载荷非法")
)
