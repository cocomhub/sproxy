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
	"fmt"
	"log/slog"
	"sync"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// Set 是装配后的卷集合：配置元数据 + 每卷打开的根句柄 + 每卷容量池 + 外部卷句柄。
// 默认卷 = cfg.Volumes[0]（defaultName 记录）；globalRoot/globalPool 语义映射到默认卷，
// 保持既有 handler 不改（globalRoot 字段 = 默认卷根，见 RegisterRoutes 接线）。
type Set struct {
	// mu 守卫 volumes 切片与 external map 的**动态写**（Add/RemoveExternalVolume）与查询：
	// 装配后本应为只读，但用户卷（U1）需要在运行时注册/移除外部卷，故 volumes/external
	// 的读写经 mu 串行化（roots/pools/caches 不被动态写，沿用既有并发模型）。
	mu sync.RWMutex
	// volumes 是装配后不可变卷描述（声明序，默认卷在 [0]），可直接作为 pkg/volume
	// AllowedVolumes/OrderCandidates 的输入（T4 路由）。
	volumes []volume.Volume
	// roots 是 name → 打开的卷根句柄（含默认卷；Close 时统一关闭）。
	// 仅本地卷（Type==""/local）在此；外部卷在 external。
	roots map[string]*storage.Root
	// external 是 name → 外部卷句柄（V3 通用卷模型）：非本地后端（如 baidupcs）的
	// 运行时句柄，FS() 提供同步视图供 syncexec 工厂消费；Close 时统一关闭。
	external map[string]ExternalBackend
	// pools 是 name → 卷容量池。Capacity<=0 仍建池（上限 0 = 不限量），便于统一入账与
	// T4 的 used(name) 活用量闭包（OrderCandidates spread）。外部卷同样建池（入账统一）。
	pools map[string]*quota.Pool
	// defaultName 是默认卷名（cfg.Volumes[0].Name）。恒等于 Default().Name（构造方由
	// assembleVolumes 保证：i==0 时取 volumes[0]）；跨包调用方一律走既有 Default().Name，
	// 故本字段不导出。
	defaultName string
	// caches 是「非默认卷名 → 该卷上的租户缓存」：每卷一个 storage.TenantCache，
	// 键为 owner（卷维度由 map 的键表达，避免跨卷句柄混用）。默认卷租户由装配层的
	// 默认卷缓存单一持有，不在此缓存（避免同路径双句柄）。
	//
	// 懒创建：第一次对某卷取租户时才建该卷的 cache；cacheMu 串行化 map 读写（cache
	// 自身的懒创建与失败关闭由 storage.TenantCache 内部加锁）。
	caches  map[string]*storage.TenantCache
	cacheMu sync.Mutex
}

// NewSet 由装配层已解码的卷集合构造运行时卷视图。
// 参数与顺序刻意与 Set 的字段一一对应（互斥量与懒建缓存由构造处自建，不作为入参），
// 使调用方（pkg/server 的 assembleVolumes）只需把装配产物交给构造处，其余逻辑不受影响。
func NewSet(
	volumes []volume.Volume,
	roots map[string]*storage.Root,
	external map[string]ExternalBackend,
	pools map[string]*quota.Pool,
	defaultName string,
) *Set {
	if external == nil {
		external = map[string]ExternalBackend{}
	}
	return &Set{
		volumes:     volumes,
		roots:       roots,
		external:    external,
		pools:       pools,
		defaultName: defaultName,
		caches:      map[string]*storage.TenantCache{},
	}
}

// Default 返回默认卷描述（volumes[0]）。
func (vs *Set) Default() volume.Volume {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return volume.DefaultVolume(vs.volumes)
}

// All 返回全部装配卷的**副本**（声明序，默认卷在 [0]）。返回副本防调用方改写内部底层数组
// 造成别名污染（volume.Volume 是值类型，切片头复制即隔离；append 到副本不影响 vs.volumes）。
func (vs *Set) All() []volume.Volume {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return append([]volume.Volume(nil), vs.volumes...)
}

// ByName 按卷名查找装配卷描述。
func (vs *Set) ByName(name string) (volume.Volume, bool) {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
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
// 外部卷（非本地）不在 roots——调用方应走 External(name)。
func (vs *Set) Root(name string) *storage.Root {
	return vs.roots[name]
}

// External 返回指定卷名的外部卷句柄（未知卷名/非外部卷返回 nil）。
func (vs *Set) External(name string) ExternalBackend {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.external[name]
}

// AddExternalVolume 在运行时注册外部卷（用户卷，U1）：追加卷元数据 + 外部句柄。
//
// 重名（volumes 中已有同名卷——本地或已注册外部卷）→ 明确错误（防卷名歧义：
// ByName/External 按名查表，同名双持有会破坏唯一性）。
//
// 不触碰 roots/pools（容量 Pool 由装配层/调用方建；动态卷池入账见 U3）。
// mu 串行化 volumes/external 的写与 All/ByName/External 的读（并发安全）。
func (vs *Set) AddExternalVolume(v volume.Volume, be ExternalBackend) error {
	if v.Name == "" {
		return fmt.Errorf("registry: 外部卷名不能为空")
	}
	if be == nil {
		return fmt.Errorf("registry: 外部卷 %q 句柄为 nil", v.Name)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for _, vv := range vs.volumes {
		if vv.Name == v.Name {
			return fmt.Errorf("registry: 外部卷 %q 已存在（卷名必须唯一）", v.Name)
		}
	}
	vs.volumes = append(vs.volumes, v)
	vs.external[v.Name] = be
	return nil
}

// RemoveExternalVolume 移除运行时注册的外部卷（用户卷，U1）：Close 后端句柄 + 删 external
// 条目 + 删 volumes 元数据。
//
// 移除不存在的卷 → 明确错误（fail-closed：静默 no-op 会掩盖调用方的卷名笔误）。
// 容量 Pool 保留（不再使用，Ruling-U1：Remove 不触碰 pools）。
func (vs *Set) RemoveExternalVolume(name string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	be, ok := vs.external[name]
	if !ok {
		return fmt.Errorf("registry: 外部卷 %q 不存在（无法移除）", name)
	}
	if be != nil {
		_ = be.Close()
	}
	delete(vs.external, name)
	for i, v := range vs.volumes {
		if v.Name == name {
			vs.volumes = append(vs.volumes[:i], vs.volumes[i+1:]...)
			break
		}
	}
	return nil
}

// Pool 返回指定卷名的容量池（未知卷名返回 nil）。
func (vs *Set) Pool(name string) *quota.Pool {
	return vs.pools[name]
}

// Close 关闭全部卷上租户子根、卷根句柄与外部卷句柄（幂等：重复调用安全，nil/已关跳过）。
// 顺序：先关各卷的租户子根（缓存持有），再关卷根与外部卷。
func (vs *Set) Close() error {
	vs.cacheMu.Lock()
	for name, c := range vs.caches {
		_ = c.Close()
		delete(vs.caches, name)
	}
	vs.cacheMu.Unlock()
	for name, rt := range vs.roots {
		if rt != nil {
			_ = rt.Close()
		}
		delete(vs.roots, name)
	}
	vs.mu.Lock()
	for name, be := range vs.external {
		if be != nil {
			_ = be.Close()
		}
		delete(vs.external, name)
	}
	vs.mu.Unlock()
	return nil
}

// Tenant 返回指定卷上 owner 的租户（懒建缓存）。未知卷名/非法 owner/根不可用返回 nil
// （fail-closed）。物理位置 = <卷根>/<owner>/（与默认卷 tenantFor 布局同构；meta 桶仅在默认
// 卷权威，非默认卷不预建 meta）。默认卷（vs.defaultName）不在此缓存——调用方应走
// server.tenantFor(owner)（默认卷缓存单一持有），避免同路径双句柄。
//
// 创建骨架与失败关闭语义单源在 pkg/storage（OpenTenant/TenantCache）：本方法只负责
// 「按卷取/建 cache」。log 仅在**首次为某卷创建 cache 时**生效（与既有「首次创建才记日志」
// 一致）；卷名以 `volume` 字段记入每条告警。
func (vs *Set) Tenant(volName, owner string, log *slog.Logger) *storage.Tenant {
	if vs.Root(volName) == nil {
		return nil
	}
	vs.cacheMu.Lock()
	c, ok := vs.caches[volName]
	if !ok {
		c = storage.NewTenantCache(vs.roots[volName],
			storage.WithLogger(log), storage.WithLogAttrs("volume", volName))
		vs.caches[volName] = c
	}
	vs.cacheMu.Unlock()
	return c.TenantFor(owner)
}
