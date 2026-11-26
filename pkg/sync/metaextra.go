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

// MetaRegistrar 是「内容已在位、按声明 size+sha256 登记 meta」的能力（presign 直传用）。
//
// 直传内容由客户端写入对象存储，服务端不重新读取内容；调用方已提供权威整文件 SHA-256。
// 实现方可生成一个覆盖整文件的校验块（不做分块细化），至少保证 TotalSHA256/Size 可校验。
// 非直传写路径不应使用该能力（普通上传应流式计算分块 meta）。
type MetaRegistrar interface { // NOSONAR: S8196 — 能力接口，设计保留
	RegisterMeta(ctx context.Context, rel string, size int64, sha256 string, mtime int64, extra map[string]any) error
}

// AssertMetaExtraUpdater 取装饰器链**最外层**的旁路 Extra 能力。
//
// 不能下探到 Innermost：trusted.Wrap 等装饰器本身就是能力提供者（为裸外部卷生成
// sidecar），下探到裸卷会恒判定无能力（WebDAV/百度/S3 的 meta set 因此 503）。
// 透明装饰器都已实现该能力并委托 inner；外层未实现时返回 ok=false。
func AssertMetaExtraUpdater(fs FS) (MetaExtraUpdater, bool) {
	if fs == nil {
		return nil, false
	}
	u, ok := fs.(MetaExtraUpdater)
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

// uploadMetaExtraKey 是 ctx 携带「随上传写入的 extra」的键（片 3：UploadSink.WriteFile
// 无 extra 参数 ⇒ 经 ctx 透传到写面 trusted/secretdata 合并进 meta）。
type uploadMetaExtraKey struct{}

// WithUploadMetaExtra 把随上传的 extra 放入 ctx（未携带 = 无额外 KV，零回归）。
func WithUploadMetaExtra(ctx context.Context, extra map[string]any) context.Context {
	if len(extra) == 0 {
		return ctx
	}
	return context.WithValue(ctx, uploadMetaExtraKey{}, extra)
}

// UploadMetaExtraFrom 取 ctx 中的随上传 extra（未携带 → nil）。
func UploadMetaExtraFrom(ctx context.Context) map[string]any {
	if m, ok := ctx.Value(uploadMetaExtraKey{}).(map[string]any); ok {
		return m
	}
	return nil
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
	_ MetaRegistrar    = (*PrefixFS)(nil)
	_ MetaRegistrar    = (*StagingQuotaGateFS)(nil)
)

// RegisterMeta 把调用方键空间映射到桶键空间后委托底层（presign 直传 meta 登记）。
func (p *PrefixFS) RegisterMeta(ctx context.Context, rel string, size int64, sha256 string, mtime int64, extra map[string]any) error {
	r, ok := p.FS.(MetaRegistrar)
	if !ok {
		return fmt.Errorf("sync: PrefixFS 底层未实现 RegisterMeta: %w", ErrUnsupported)
	}
	return r.RegisterMeta(ctx, p.Key(rel), size, sha256, mtime, extra)
}
