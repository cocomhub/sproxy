// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
)

// volumes_enclosure.go 是**封装卷的装配接线**（两部分）：
//
//  1. **封印生效**：解析封装声明，把封印规则注入**源卷根**（有效路径按封装链拼接），使源
//     目录在卷根命名空间下无法被直连读写（写恒拒、读按声明 hidden/readable）。封印随后
//     自动传播到该卷的每个租户子根（pkg/storage/tenant_resolve.go）。
//  2. **配额继承**：封装卷容量池挂到**有效源卷池**之下（默认仅受源卷约束 = 继承；配
//     max_bytes 则自身上限与源卷上限双重约束），复用既有 quota 父链（不双计）。
//
// 装配顺序不变量（设计 §3）：封印必须在「封装卷已取得特权源视图」之后注入。本函数在卷循环
// 结束后、NewSet 之前执行；封装卷后端本身在此之后补装（enclosure.AttachAll，推迟装配），
// 其源视图直接持有源 FS（不受封印影响）。
//
// 跨外部卷源（baidupcs/s3/…）：封印经 `enclosure.SealBackend` 套在**该后端 FS 上**（装配处
// 就地包装）⇒ 所有 `be.FS()` 调用者自动继承封印；封装卷自身经 `enclosure.UnsealedSource`
// 取未封印特权视图（只剥封印层，配额/可信装饰器保留）。

// applyEnclosureSeals 按封装声明注入封印 + 配额继承（见文件头）。
func applyEnclosureSeals(log *slog.Logger, acc *volumeAccum) error {
	decls, err := enclosure.Parse(acc.volumes)
	if err != nil {
		return fmt.Errorf("封装卷声明校验失败（fail-closed）: %w", err)
	}
	if len(decls) == 0 {
		return nil // 无封装声明的部署：行为零回归
	}
	if log == nil {
		log = slog.Default()
	}

	if serr := injectSeals(acc, decls); serr != nil {
		return serr
	}
	inheritPools(acc, decls)
	logEnclosureDecls(log, decls)
	return nil
}

// injectSeals 按**有效目标**（封装链拼接后的卷与路径）向源卷根注入封印，嵌套时逐层叠加。
func injectSeals(acc *volumeAccum, decls []enclosure.Decl) error {
	byVol := make(map[string][]storage.SealRule, len(decls))
	for _, d := range decls {
		baseVol, rules, ferr := enclosure.EffectiveFootprint(d, decls)
		if ferr != nil {
			return fmt.Errorf("封装卷 %q 封印足迹解析失败（fail-closed）: %w", d.Vol, ferr)
		}
		if rt := acc.roots[baseVol]; rt != nil {
			byVol[baseVol] = append(byVol[baseVol], rules...)
			continue
		}
		// **外部/远端源卷**（s3/baidupcs/pikpak/cluster/secretdata/…）：无 storage.Root ⇒ 封印
		// 只能套在该后端 FS 的**装配处**（此处）——`be.FS()` 的十余处调用者因此全部继承封印，
		// 这是外部卷封印唯一可靠的收口点（逐面打补丁必漏，见设计 §6「强制点在公共底座」）。
		be := acc.external[baseVol]
		if be == nil {
			return fmt.Errorf("封装卷 %q 的有效源卷 %q 未装配（既无本地根也无外部后端）：无法注入封印，拒绝启动（禁静默降级）",
				d.Vol, baseVol)
		}
		acc.external[baseVol] = enclosure.SealBackend(be, rules)
	}
	for name, rules := range byVol {
		if serr := acc.roots[name].SetSeals(rules); serr != nil {
			return fmt.Errorf("向源卷 %q 注入封印失败（fail-closed）: %w", name, serr)
		}
	}
	return nil
}

// inheritPools 让封装卷容量池继承**有效源卷池**（默认仅受源卷约束；配 max_bytes 双重约束）。
func inheritPools(acc *volumeAccum, decls []enclosure.Decl) {
	for _, d := range decls {
		baseVol, _ := enclosure.EffectiveTarget(d, decls)
		srcPool := acc.pools[baseVol]
		if srcPool == nil {
			continue // 源卷无池（外部卷推迟装配等）：不阻塞，后续装配可再挂
		}
		acc.pools[d.Vol] = quota.NewPoolWithParent(srcPool, d.MaxBytes)
	}
}

// logEnclosureDecls 逐条打印封装声明生效状态（禁静默降级：封印语义必须可观测）。
func logEnclosureDecls(log *slog.Logger, decls []enclosure.Decl) {
	for _, d := range decls {
		baseVol, basePath := enclosure.EffectiveTarget(d, decls)
		mode := "hidden(读404+列举过滤)"
		if d.ReadMode == storage.SealReadable {
			mode = "readable(只读透视，写仍禁止)"
		}
		log.Info("封装卷声明生效（源目录已封印，禁止直连）",
			"enclosure", d.Vol, "wrapper", d.Wrapper,
			"source", d.Source, "path", d.Path,
			"effective_source", baseVol, "effective_path", basePath,
			"read_mode", mode, "allow_direct_url", d.AllowDirectURL, "max_bytes", d.MaxBytes)
	}
}

// isEnclosureDecl 报告卷声明是否走**封装装配**：显式 `type: enclosure` 或嵌套 wrapper 形态
// （wrapper 类型 + `extra.source`）。判据单源在 `enclosure.IsDeclared`（装配词汇），本函数只做
// 配置层（VolumeConfig）到卷描述的适配——装配期据此把此类卷排除在按类型分派的
// `registry.NewBackend` 之外（后端由 `enclosure.AttachAll` 在卷集就绪后构造）。
func isEnclosureDecl(typ string, extra map[string]any) bool {
	return enclosure.IsDeclared(volume.Volume{Type: typ, Extra: extra})
}
