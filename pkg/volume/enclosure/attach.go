// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// attach.go 负责**推迟装配**（deferred）：封装卷的后端构造依赖「已装配的源卷集」
// （需要源卷的特权视图 FS），而 assembleVolumes 执行时 set 尚未就绪 —— 与 secretdata 同型
// 时序问题。故：
//   - 装配期：仅登记卷元数据 + 容量池（registry.MarkDeferredType），并注入源卷封印；
//   - set 就绪后：AttachAll 按声明拓扑序构造后端（Decl.Build）并挂回 Set.External。
func MarkDeferred() { registry.MarkDeferredType(TypeEnclosure) }

// EffectiveTarget 沿封装链回溯出**最终落盘的（本地/外部）卷与路径**：
// 嵌套时路径逐层拼接（内层 Path + 外层 Path），使封印与容量归集落在真正承载字节的那一层。
//
// 例：inner 封装 main:base，outer 封装 inner:sub → 有效目标 (main, base/sub)。
func EffectiveTarget(d Decl, all []Decl) (baseVol, basePath string) {
	byVol := make(map[string]Decl, len(all))
	for _, x := range all {
		byVol[x.Vol] = x
	}
	cur, p := d, strings.Trim(d.Path, "/")
	for {
		next, ok := byVol[cur.Source]
		if !ok {
			return cur.Source, p
		}
		nextPath := strings.Trim(next.Path, "/")
		switch {
		case nextPath == "":
			// 内层整卷封装：路径不变
		case p == "":
			p = nextPath
		default:
			p = path.Join(nextPath, p)
		}
		cur = next
	}
}

// AttachOptions 是补装的装配参数（可选；零值 = 与旧行为一致）。
type AttachOptions struct {
	// StorageRoot 是 count/状态文件所在根（用于**持久化**卷容量计数器）。为空则计数器不持久
	// （重启后封装卷 `max_bytes` 的 committed 会归零——生产必须传）。
	StorageRoot string
}

// AttachAll 为所有已登记 `type: enclosure` 卷构造后端并挂回卷集（幂等：已挂者跳过）。
// 必须在**卷集就绪后**调用（与 secretdata 补装同层，fail-closed：任一卷失败返回错误）。
// opts.StorageRoot 非空时启用持久化容量计数器（与外部卷同源）。
func AttachAll(ctx context.Context, set *registry.Set, logger *slog.Logger, opts ...AttachOptions) error {
	var opt AttachOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	return attachAllWithOpts(ctx, set, logger, opt)
}

// attachAllWithOpts 是 AttachAll 的实现（选项显式传入，供内部与测试使用）。
func attachAllWithOpts(ctx context.Context, set *registry.Set, logger *slog.Logger, opt AttachOptions) error {
	if set == nil {
		return fmt.Errorf("enclosure: 卷集未装配")
	}
	decls, err := Parse(set.All())
	if err != nil {
		return fmt.Errorf("enclosure: 声明校验失败: %w", err)
	}
	log := slog.Default()
	if logger != nil {
		log = logger
	}
	for _, d := range decls {
		if err := attachOne(ctx, set, d, opt); err != nil {
			return err
		}
		baseVol, basePath := EffectiveTarget(d, decls)
		log.Info("封装卷后端补装完成（经服务器处理读写；源目录已封印）",
			"enclosure", d.Vol, "wrapper", d.Wrapper, "source", d.Source, "path", d.Path,
			"effective_source", baseVol, "effective_path", basePath)
	}
	return nil
}

// attachOne 补装单个封装卷（已装配则跳过）。
func attachOne(ctx context.Context, set *registry.Set, d Decl, opt AttachOptions) error {
	if set.External(d.Vol) != nil {
		return nil // 已装配（测试自注册类型等）
	}
	v, ok := set.ByName(d.Vol)
	if !ok {
		return fmt.Errorf("enclosure: 卷 %q 未登记", d.Vol)
	}
	be, berr := BuildBackend(ctx, set, d, v, opt)
	if berr != nil {
		return berr
	}
	if aerr := set.AttachExternal(d.Vol, be); aerr != nil {
		return fmt.Errorf("enclosure: 封装卷 %q 挂回失败: %w", d.Vol, aerr)
	}
	return nil
}

// BuildBackend 构造封装卷后端（源特权视图 → wrapper → 容量计数器包装），**不挂回卷集**。
//
// 挂回由调用方决定：装配期用 `Set.AttachExternal`（卷元数据/池已在装配期登记），运行期新增
// 封装卷用 `Set.AddExternalVolume`（元数据与句柄一次性登记）。两者共用本函数，保证
// 「源视图取法 + 容量记账接法」不出现第二套口径。
func BuildBackend(ctx context.Context, set *registry.Set, d Decl, v volume.Volume, opt AttachOptions) (registry.ExternalBackend, error) {
	src, serr := SourceFS(set, d)
	if serr != nil {
		return nil, serr
	}
	be, berr := d.Build(ctx, v, src)
	if berr != nil {
		return nil, fmt.Errorf("enclosure: 封装卷 %q 装配失败: %w", d.Vol, berr)
	}
	// 容量记账：封装卷池在装配期/运行期已挂到有效源卷池之下（继承语义），此处把后端包上
	// CapacityFS ⇒ `max_bytes` 与用量统计真实生效（写入按声明大小预留/结算；卷层实现
	// TrueUsageReporter 时真值优先，见 pkg/volume/capacity/trueusage.go）。
	pool := set.Pool(d.Vol)
	if pool == nil {
		return be, nil
	}
	// 计数器**持久化**（与外部卷同源，`capacity.VolumeCounterPath`）：此前用非持久
	// `NewPoolCounter`，而封装卷 `RootDir==""` 被 `reconcileVolumesFromDisk` 跳过 ⇒
	// 重启后 committed 归零、`max_bytes` 被突破（评审 P1）。
	if opt.StorageRoot != "" {
		if counter, cerr := capacity.NewPoolCounterPersistent(pool, capacity.VolumeCounterPath(opt.StorageRoot, d.Vol)); cerr == nil {
			return capacity.WrapBackend(be, counter), nil
		}
	}
	return capacity.WrapBackend(be, capacity.NewPoolCounter(pool)), nil
}

// SourceFS 取源卷的**特权视图**（封印前语义：直接 raw 视图，不受封印影响——封装卷必须能
// 读写被封区间；客户端则只能经封装卷由服务器处理）。
//
//   - 本地源：`NewLocalFS(卷根绝对路径)`（与 DAV/旧 secretdata 同型；封印在 storage.Root
//     层，LocalFS 绕开封印正是「特权」的含义）；
//   - 封装源（嵌套）：内层封装卷的后端 FS（已是带前缀视图，外层再叠自己的前缀）；
//   - 其它外部源（baidupcs/s3/…）：后端 FS 的**未封印**视图（封印层由装配层在
//     applyEnclosureSeals 中套在该后端的 FS 上；此处只剥封印层 ⇒ 特权）。
func SourceFS(set *registry.Set, d Decl) (syncpkg.FS, error) {
	v, ok := set.ByName(d.Source)
	if !ok {
		return nil, fmt.Errorf("enclosure: 源卷 %q 不存在（卷 %q）", d.Source, d.Vol)
	}
	if v.Type == "" || v.Type == volume.TypeLocal {
		rt := set.Root(d.Source)
		if rt == nil {
			return nil, fmt.Errorf("enclosure: 源卷 %q 根未打开（卷 %q）", d.Source, d.Vol)
		}
		return syncpkg.NewLocalFS(rt.AbsPath(), nil), nil
	}
	if v.Type == TypeEnclosure {
		be := set.External(d.Source)
		if be == nil {
			return nil, fmt.Errorf("enclosure: 内层封装卷 %q 后端未装配（卷 %q；装配顺序应为源在前）", d.Source, d.Vol)
		}
		return be.FS(), nil
	}
	// 其它外部/远端源（s3/baidupcs/pikpak/cluster/secretdata/…）：封印（若装配层已为该源卷
	// 注入，见 applyEnclosureSeals 对 acc.external 的包装）位于 FS 装饰器链上 ⇒ 取**未封印**
	// 特权视图（只剥封印层，配额/可信装饰器保留）；封装自身读写被判区间不受封印影响。
	be := set.External(d.Source)
	if be == nil {
		return nil, fmt.Errorf("enclosure: 源卷 %q 后端未装配（卷 %q；装配顺序应为源在前）", d.Source, d.Vol)
	}
	return UnsealedSource(be), nil
}
