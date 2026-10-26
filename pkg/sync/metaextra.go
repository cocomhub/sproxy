// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
)

// metaextra.go 声明**旁路 Extra 更新**能力接口 `MetaExtraUpdater`（此前只散落在
// pkg/volume/trusted（本地声明）与 pkg/cloud（结构化断言），未纳入 pkg/sync 能力枚举
// ⇒ R21 门禁无法覆盖，装饰器漏转发不会被发现——第 9 轮评审已指出该盲区）。
//
// 语义：在不重写主文件的前提下更新其 meta sidecar 的 Extra 字段（damaged 源完整性、
// transfer_verified 等旁路标记）。**透明装饰器必须无条件转发**（否则上游拿不到能力 =
// 标记静默丢失 = 完整性语义降级）。
type MetaExtraUpdater interface {
	// UpdateMetaExtra 更新 rel 的 sidecar Extra（合并语义由实现方定义；rel 为调用方键空间）。
	UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
}

// AssertMetaExtraUpdater 取装饰器链上的旁路 Extra 能力（沿 Inner() 下探到最内层后断言；
// 未实现返回 ok=false——调用方自行决定降级/告警，不得静默忽略）。
func AssertMetaExtraUpdater(fs FS) (MetaExtraUpdater, bool) {
	if fs == nil {
		return nil, false
	}
	u, ok := Innermost(fs).(MetaExtraUpdater)
	return u, ok
}

// MetaUpdateMode 是 meta extra 更新模式（用户裁定 2026-10-26：默认**新增而非覆盖**）。
type MetaUpdateMode string

const (
	// MetaUpdateMerge 合并（同名键更新，其余保留）——**默认**（零值）。
	MetaUpdateMerge MetaUpdateMode = "merge"
	// MetaUpdateReplace 整表替换（丢弃所有旧键）。
	MetaUpdateReplace MetaUpdateMode = "replace"
	// MetaUpdateDelete 只删指定键（extra 的 key 集合，value 忽略）。
	MetaUpdateDelete MetaUpdateMode = "delete"
)

// metaUpdateModeKey 是 ctx 携带更新模式的键（不扩 UpdateMetaExtra 接口签名，避免 11 个
// 装饰器实现逐一改签名；也未新的可选能力接口——模式是一次调用的参数，不是能力）。
type metaUpdateModeKey struct{}

// WithMetaUpdateMode 把更新模式放入 ctx（未携带 = MetaUpdateMerge）。
func WithMetaUpdateMode(ctx context.Context, mode MetaUpdateMode) context.Context {
	if mode == "" {
		mode = MetaUpdateMerge
	}
	return context.WithValue(ctx, metaUpdateModeKey{}, mode)
}

// MetaUpdateModeFrom 取 ctx 中的更新模式（未携带 → MetaUpdateMerge）。
func MetaUpdateModeFrom(ctx context.Context) MetaUpdateMode {
	if m, ok := ctx.Value(metaUpdateModeKey{}).(MetaUpdateMode); ok && m != "" {
		return m
	}
	return MetaUpdateMerge
}

// UpdateMetaExtra 把调用方键空间（相对路径）映射到桶键空间后委托底层（见文件头契约）。
// 底层未实现 → ErrUnsupported（能力探测语义：未实现与「空操作」必须可区分）。
func (p *PrefixFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	u, ok := p.FS.(MetaExtraUpdater)
	if !ok {
		return fmt.Errorf("sync: PrefixFS 底层未实现 UpdateMetaExtra: %w", ErrUnsupported)
	}
	return u.UpdateMetaExtra(ctx, p.Key(rel), extra)
}

// 编译期能力断言（R21 门禁要求：pkg/sync 新增能力接口后，登记装饰器必须声明或显式豁免）。
var (
	_ MetaExtraUpdater = (*PrefixFS)(nil)
	_ MetaExtraUpdater = (*StagingQuotaGateFS)(nil)
)
