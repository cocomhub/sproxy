// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package registry 是 pkg/volume 域下的**运行时卷集合**子包：持有装配完成后的卷视图
// （不可变卷描述 + 每卷已打开的根句柄 + 每卷容量池 + 卷上租户懒建缓存），并提供按卷名/
// 默认卷的查询与统一关闭。
//
// 边界（本包刻意不承担的部分）：
//   - **配置解析**不在本包：把服务端配置（*Config / *VolumeACLConfig）翻译成纯域类型的
//     resolveDefaultVolumeRoot / assembleVolumes / parseVolumeACL 留在 pkg/server——
//     搬进来要连配置类型一起搬，且会造成 registry → pkg/server 成环。
//   - **卷路由决策**不在本包：routeUpload / locateForRead / volumeTenant 等 (h *Handlers)
//     方法消费本包，但自身需要 Handlers 状态（配额 Scope、日志、配置热读），留在 pkg/server。
package registry

import (
	"log/slog"
	"os"
	"sync"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// defaultLogger 返回一个有效的 *slog.Logger。
// 当 l 为 nil 时返回 slog.Default()，否则原样返回。
//
// 随包搬迁的私有依赖：原先位于 pkg/server/slogger.go，抽取后本包不能反向导入
// pkg/server，故连同被搬代码一起带上。逐字先例是 pkg/checksum 与 pkg/storage/capacity
// 的同名私有辅助（搬运时同样带上）。
func defaultLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// Set 是装配后的卷集合：配置元数据 + 每卷打开的根句柄 + 每卷容量池。
// 默认卷 = cfg.Volumes[0]（defaultName 记录）；globalRoot/globalPool 语义映射到默认卷，
// 保持既有 handler 不改（globalRoot 字段 = 默认卷根，见 RegisterRoutes 接线）。
type Set struct {
	// volumes 是装配后不可变卷描述（声明序，默认卷在 [0]），可直接作为 pkg/volume
	// AllowedVolumes/OrderCandidates 的输入（T4 路由）。
	volumes []volume.Volume
	// roots 是 name → 打开的卷根句柄（含默认卷；Close 时统一关闭）。
	roots map[string]*storage.Root
	// pools 是 name → 卷容量池。Capacity<=0 仍建池（上限 0 = 不限量），便于统一入账与
	// T4 的 used(name) 活用量闭包（OrderCandidates spread）。
	pools map[string]*quota.Pool
	// defaultName 是默认卷名（cfg.Volumes[0].Name）。恒等于 Default().Name（构造方由
	// assembleVolumes 保证：i==0 时取 volumes[0]）；跨包调用方一律走既有 Default().Name，
	// 故本字段不导出。
	defaultName string
	// tenants 是 (非默认) 卷 × owner 的租户懒建缓存：key = volName + "\x00" + owner。
	// 默认卷租户由 server.tenantRoots（tenantFor）单一持有，不在此缓存（避免同路径双句柄）。
	// tenantMu 串行化懒建（与 Close 并发时保护 map）。
	tenants  map[string]*storage.Tenant
	tenantMu sync.Mutex
}

// NewSet 由装配层已解码的卷集合构造运行时卷视图。
// 参数与顺序刻意与 Set 的字段一一对应（tenantMu 除外——互斥量恒零值起步，不作为构造入参），
// 使调用方（pkg/server 的 assembleVolumes）只需把装配产物交给构造处，其余逻辑不受影响。
func NewSet(
	volumes []volume.Volume,
	roots map[string]*storage.Root,
	pools map[string]*quota.Pool,
	defaultName string,
	tenants map[string]*storage.Tenant,
) *Set {
	return &Set{
		volumes:     volumes,
		roots:       roots,
		pools:       pools,
		defaultName: defaultName,
		tenants:     tenants,
	}
}

// Default 返回默认卷描述（volumes[0]）。
func (vs *Set) Default() volume.Volume {
	return volume.DefaultVolume(vs.volumes)
}

// All 返回全部装配卷的**副本**（声明序，默认卷在 [0]）。返回副本防调用方改写内部底层数组
// 造成别名污染（volume.Volume 是值类型，切片头复制即隔离；append 到副本不影响 vs.volumes）。
func (vs *Set) All() []volume.Volume {
	return append([]volume.Volume(nil), vs.volumes...)
}

// ByName 按卷名查找装配卷描述。
func (vs *Set) ByName(name string) (volume.Volume, bool) {
	for _, v := range vs.volumes {
		if v.Name == name {
			return v, true
		}
	}
	return volume.Volume{}, false
}

// DefaultRoot 返回默认卷的根句柄（nil = 未装配/空集合）。
func (vs *Set) DefaultRoot() *storage.Root {
	return vs.roots[vs.defaultName]
}

// Root 返回指定卷名的根句柄（未知卷名返回 nil）。
func (vs *Set) Root(name string) *storage.Root {
	return vs.roots[name]
}

// Pool 返回指定卷名的容量池（未知卷名返回 nil）。
func (vs *Set) Pool(name string) *quota.Pool {
	return vs.pools[name]
}

// Close 关闭全部卷根句柄（幂等：重复调用安全，nil/已关跳过）。
func (vs *Set) Close() error {
	vs.tenantMu.Lock()
	for key, t := range vs.tenants {
		if t != nil && t.Root() != nil {
			_ = t.Root().Close()
		}
		delete(vs.tenants, key)
	}
	vs.tenantMu.Unlock()
	for name, rt := range vs.roots {
		if rt != nil {
			_ = rt.Close()
		}
		delete(vs.roots, name)
	}
	return nil
}

// Tenant 返回指定卷上 owner 的租户（懒建缓存）。未知卷名/非法 owner/根不可用返回 nil
// （fail-closed）。物理位置 = <卷根>/<owner>/（与默认卷 tenantFor 布局同构；meta 桶仅在默认
// 卷权威，非默认卷不预建 meta）。默认卷（vs.defaultName）不在此缓存——调用方应走
// server.tenantFor(owner)（既有 tenantRoots 缓存单一持有），避免同路径双句柄。
func (vs *Set) Tenant(volName, owner string, log *slog.Logger) *storage.Tenant {
	log = defaultLogger(log)
	key := volName + "\x00" + owner
	vs.tenantMu.Lock()
	defer vs.tenantMu.Unlock()
	if t, ok := vs.tenants[key]; ok {
		return t
	}
	rt := vs.roots[volName]
	if rt == nil {
		return nil
	}
	if !storage.ValidSegmentName(owner) {
		log.Warn("非法租户名，拒绝在卷上创建（fail-closed）", "volume", volName, "owner", owner)
		return nil
	}
	abs, ok := rt.Abs(owner)
	if !ok {
		log.Warn("租户路径越界，拒绝在卷上创建（fail-closed）", "volume", volName, "owner", owner)
		return nil
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		log.Warn("创建卷上租户根目录失败", "volume", volName, "owner", owner, "error", err)
		return nil
	}
	tenantRoot, err := storage.OpenRoot(abs)
	if err != nil {
		log.Warn("打开卷上租户子根失败（fail-closed）", "volume", volName, "owner", owner, "error", err)
		return nil
	}
	t, err := storage.NewTenant(owner, tenantRoot)
	if err != nil {
		log.Warn("创建卷上租户失败（fail-closed）", "volume", volName, "owner", owner, "error", err)
		_ = tenantRoot.Close()
		return nil
	}
	vs.tenants[key] = t
	return t
}
