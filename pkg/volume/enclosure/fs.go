// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"fmt"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fs.go 是**封装卷视图的防护薄层**：组合（不复制）`sync.PrefixFS` 作为源视图内核，
// 只补三件事（设计 §5）：
//  1. **抑制 DirectURL**：默认禁止 302 直链——直链意味着客户端直连源存储，与「封装后源
//     目录只能经服务器处理」冲突（会读到密文/原始存储形态）；仅 `allow_direct_url: true`
//     显式放开。
//  2. **能力透传**：`MetaExtraUpdater` 等旁路能力由内嵌 PrefixFS 透明转发（R21 断言见下）。
//  3. **身份归因**：暴露封装身份（wrapper/源卷/子路径）供可观测与错误信息。
type FS struct {
	*syncpkg.PrefixFS
	decl Decl
}

// NewFS 组装封装视图：源 FS + 声明 → 键映射视图 + 防护层。
// src 必须是**源卷特权视图**（封印注入前取得；封装卷自身不受封印影响）。
//
// **本视图是「wrapper 面」**（区域相对键：封装处理器在区域内自建容器/blob 名，见 ownerfs.go 的
// 两个视图说明）；**调用方面**的 owner 映射视图由 `WrapOwnerFacing` 装在 `backend.FS()` 上
// （`Decl.Build` 内完成）。二者不可合并（CI 实证：单一映射无法同时满足两种键空间）。
func NewFS(src syncpkg.FS, d Decl) *FS {
	return &FS{PrefixFS: syncpkg.NewPrefixFS(src, d.KeySpacePrefix()), decl: d}
}

// Decl 返回封装声明（诊断/配额继承/封印规则生成用）。
func (f *FS) Decl() Decl { return f.decl }

// Identity 返回封装身份的稳定描述（日志/错误归因）。
func (f *FS) Identity() string {
	return fmt.Sprintf("enclosure(%s wrapper=%s source=%s path=%q)", f.decl.Vol, f.decl.Wrapper, f.decl.Source, f.decl.Path)
}

// SourceView 返回源视图（PrefixFS 层本身），供装配层注入封印前建立/校验。
func (f *FS) SourceView() syncpkg.FS { return f.PrefixFS }

// DirectURL 默认**抑制**直链（返回 ok=false：抑制是策略而非故障）。
// 语义要点：
//   - 只有显式 `allow_direct_url: true` 才透传底层直链；
//   - 透传时必须以**桶键空间**询问底层（与其余方法一致），否则底层找不到对象会误判「无直链」；
//   - 底层未实现 → 与既有装饰器同口径返回 ErrUnsupported（能力探测语义）。
func (f *FS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if !f.decl.AllowDirectURL {
		return "", false, nil
	}
	view := f.PrefixFS
	dp, ok := view.FS.(syncpkg.DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("enclosure: 源视图未实现 DirectURL: %w", syncpkg.ErrUnsupported)
	}
	return dp.DirectURL(ctx, view.Key(relPath))
}

// 编译期能力断言（R21：封装视图必须透明转发全部可选能力；BlockAccessor 与其余装饰器
// 同口径豁免——引擎回退语义需 Capability 探测）。
// R21 已登记装饰器的断言清单以 internal/archcheck 的 decorators 为准（本包已登记）；
// 内嵌 *PrefixFS 使这些能力由 PrefixFS 透明提供（含 MetaExtraUpdater/WriteIfAbsent/
// StagedPathReuse/ReserveSpace/Mover/Copier/Linker/StagingQuota*/LocalVolume/RangeReader）。
// 能力断言：内嵌 *PrefixFS 提供全部可选能力（视图只搬键；防护层只补 DirectURL 抑制）。
// BlockAccessor 与其余装饰器同口径豁免（引擎回退语义需 Capability 探测）。
var (
	_ syncpkg.FS                  = (*FS)(nil)
	_ syncpkg.RangeReader         = (*FS)(nil)
	_ syncpkg.DirectURLProvider   = (*FS)(nil)
	_ syncpkg.WriteIfAbsent       = (*FS)(nil)
	_ syncpkg.ReserveSpace        = (*FS)(nil)
	_ syncpkg.LocalVolume         = (*FS)(nil)
	_ syncpkg.Mover               = (*FS)(nil)
	_ syncpkg.Copier              = (*FS)(nil)
	_ syncpkg.Linker              = (*FS)(nil)
	_ syncpkg.StagingQuotaCapable = (*FS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*FS)(nil)
	_ syncpkg.StagedPathReuse     = (*FS)(nil)
	_ syncpkg.MetaExtraUpdater    = (*FS)(nil)
	_ meta.Provider               = (*FS)(nil)
)

// ListDirNames 转发轻量列举（未实现时回落 ListDir）。
func (f *FS) ListDirNames(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	// 直接委托内嵌 PrefixFS（其 ListDirNames 已实现「转发或用回落 ListDir」，
	// 符合 R22「业务代码不得下探内层 FS」）。
	return f.PrefixFS.ListDirNames(ctx, p)
}

var (
	_ syncpkg.NameLister = (*FS)(nil)
)

// 编译期能力断言（门禁 R21）：透明装饰器必须声明全部 `pkg/sync` 可选能力，否则上层
// `fs.(能力)` 断言恒 false ⇒ **静默降级**（批量校验 / 秒传刷新被悄悄跳过）。
var (
	_ syncpkg.BatchVerifier  = (*FS)(nil)
	_ syncpkg.RapidRefresher = (*FS)(nil)
)

// VerifyMany 转发批量校验（透明装饰器必须转发，否则上层 `AssertBatchVerifier` 恒 false）。
//
// 底层未实现该能力 ⇒ 返回 **nil**：调用方（secretdata.verifyBatchAndRepair）已有「结果长度
// 与请求不符 ⇒ 无法判定 ⇒ 跳过批量校验」的防御分支，且 Put 内部本就逐块即时复核 ⇒ 零回归。
// **不得**返回「全部 mismatch」——那会被读成需要重传而触发无谓重传。
func (f *FS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	// 内层是**具体类型** *syncpkg.PrefixFS（其自身转发该能力）⇒ 直接调用，无需断言。
	return f.PrefixFS.VerifyMany(ctx, items)
}

// RapidRefresh 转发秒传刷新（零传输刷新远端 meta）；底层未实现 ⇒ ErrUnsupported（调用方回落重传）。
func (f *FS) RapidRefresh(ctx context.Context, item syncpkg.VerifyItem) (bool, error) {
	return f.PrefixFS.RapidRefresh(ctx, item)
}

// ---- 存储审查能力转发（2026-11-11 能力下沉 pkg/sync 后必须转发）----
//
// 为什么必须转发：装饰器不实现这两个接口时，上层 `fs.(StorageInspector)` 断言恒 false ⇒
// **静默降级**（把"底层支持审查"误判为"不支持"）。本类型内嵌 `*syncpkg.PrefixFS`（其 Inner()
// 已提升），故直接取内嵌 `PrefixFS` 下探（不调用解包方法，R22 门禁守）；底层未实现 ⇒ 返回 ErrUnsupported 交由调用方处置。
func (f *FS) Trace(ctx context.Context, rel string, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	if v, ok := any(f.PrefixFS).(syncpkg.StorageInspector); ok {
		return v.Trace(ctx, rel, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

func (f *FS) Explain(ctx context.Context, realRel string, mode syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
	if v, ok := any(f.PrefixFS).(syncpkg.StorageInspector); ok {
		return v.Explain(ctx, realRel, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

func (f *FS) TraceTree(ctx context.Context, rel string, depth int, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	if v, ok := any(f.PrefixFS).(syncpkg.TreeInspector); ok {
		return v.TraceTree(ctx, rel, depth, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

// CleanupStaleDirMeta 转发目录 meta 残留清理（2026-11-12 能力下沉 pkg/sync 后必须转发）。
//
// 与审查能力同口径：不转发 ⇒ 上层 `fs.(syncpkg.DirMetaCleaner)` 断言恒 false ⇒ 静默降级。
func (f *FS) CleanupStaleDirMeta(ctx context.Context, container string) (int, error) {
	if c, ok := any(f.PrefixFS).(syncpkg.DirMetaCleaner); ok {
		return c.CleanupStaleDirMeta(ctx, container)
	}
	return 0, syncpkg.ErrUnsupported
}

var (
	_ syncpkg.StorageInspector = (*FS)(nil)
	_ syncpkg.TreeInspector    = (*FS)(nil)
	_ syncpkg.DirMetaCleaner   = (*FS)(nil)
)
