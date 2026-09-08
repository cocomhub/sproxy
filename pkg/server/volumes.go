// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes.go 实现多卷存储的 server 侧装配（任务 3）：把 cfg.Volumes 逐卷打开根 + 建卷容量池
// + ACL 解析为 pkg/volume 纯域类型，并处理 F1 缺省卷根裁决（storage_root 与占位 volumes 的解耦）。
//
// 默认卷 = cfg.Volumes[0]；globalRoot/tenantFor 等既有 handler 语义映射到默认卷根，本任务不把
// 写路径切到卷感知（T4 起），因此单卷零回归可测。

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sync"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// resolveDefaultVolumeRoot 裁决默认卷（Volumes[0]）的物理挂载根（F1 合入门禁）。
//
// 背景：Default()/SetDefaults 在用户未配 volumes 时预合成单默认卷，其 Root 停留在占位
// defaultStorageRoot（"./storage"）。此时若用户显式配置了 storage_root（如 /data），装配按
// Volumes[0].Root 建根会把存储根静默漂移到 ./storage（单卷零回归红线）。
//
// 规则：Volumes[0].Root == defaultStorageRoot（占位形态，即用户未显式给首卷 root）时用
// cfg.StorageRoot 建默认卷根；首卷显式非占位 root 用其本身。第二卷起不经本裁决，直接用各自
// Volumes[i].Root（assembleVolumes 循环内仅 i==0 调用本函数）。
func resolveDefaultVolumeRoot(cfg *Config) string {
	if len(cfg.Volumes) == 0 {
		return cfg.StorageRoot
	}
	if cfg.Volumes[0].Root == defaultStorageRoot {
		return cfg.StorageRoot
	}
	return cfg.Volumes[0].Root
}

// volumeSet 是装配后的卷集合：配置元数据 + 每卷打开的根句柄 + 每卷容量池。
// 默认卷 = cfg.Volumes[0]（defaultName 记录）；globalRoot/globalPool 语义映射到默认卷，
// 保持既有 handler 不改（globalRoot 字段 = 默认卷根，见 RegisterRoutes 接线）。
type volumeSet struct {
	// volumes 是装配后不可变卷描述（声明序，默认卷在 [0]），可直接作为 pkg/volume
	// AllowedVolumes/OrderCandidates 的输入（T4 路由）。
	volumes []volume.Volume
	// roots 是 name → 打开的卷根句柄（含默认卷；Close 时统一关闭）。
	roots map[string]*storage.Root
	// pools 是 name → 卷容量池。Capacity<=0 仍建池（上限 0 = 不限量），便于统一入账与
	// T4 的 used(name) 活用量闭包（OrderCandidates spread）。
	pools map[string]*quota.Pool
	// defaultName 是默认卷名（cfg.Volumes[0].Name）。
	defaultName string
	// tenants 是 (非默认) 卷 × owner 的租户懒建缓存：key = volName + "\x00" + owner。
	// 默认卷租户由 server.tenantRoots（tenantFor）单一持有，不在此缓存（避免同路径双句柄）。
	// tenantMu 串行化懒建（与 Close 并发时保护 map）。
	tenants  map[string]*storage.Tenant
	tenantMu sync.Mutex
}

// Default 返回默认卷描述（volumes[0]）。
func (vs *volumeSet) Default() volume.Volume {
	return volume.DefaultVolume(vs.volumes)
}

// All 返回全部装配卷的**副本**（声明序，默认卷在 [0]）。返回副本防调用方改写内部底层数组
// 造成别名污染（volume.Volume 是值类型，切片头复制即隔离；append 到副本不影响 vs.volumes）。
func (vs *volumeSet) All() []volume.Volume {
	return append([]volume.Volume(nil), vs.volumes...)
}

// ByName 按卷名查找装配卷描述。
func (vs *volumeSet) ByName(name string) (volume.Volume, bool) {
	for _, v := range vs.volumes {
		if v.Name == name {
			return v, true
		}
	}
	return volume.Volume{}, false
}

// DefaultRoot 返回默认卷的根句柄（nil = 未装配/空集合）。
func (vs *volumeSet) DefaultRoot() *storage.Root {
	return vs.roots[vs.defaultName]
}

// Root 返回指定卷名的根句柄（未知卷名返回 nil）。
func (vs *volumeSet) Root(name string) *storage.Root {
	return vs.roots[name]
}

// Pool 返回指定卷名的容量池（未知卷名返回 nil）。
func (vs *volumeSet) Pool(name string) *quota.Pool {
	return vs.pools[name]
}

// Close 关闭全部卷根句柄（幂等：重复调用安全，nil/已关跳过）。
func (vs *volumeSet) Close() error {
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
func (vs *volumeSet) Tenant(volName, owner string, log *slog.Logger) *storage.Tenant {
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

// assembleVolumes 按 cfg.Volumes 装配卷集合：逐卷 MkdirAll + storage.OpenRoot（LAYOUT_VERSION
// 校验/写入） + 卷容量 Pool + ACL 解析。首卷物理根按 resolveDefaultVolumeRoot 裁决（F1）。
// 任一卷根打开失败即整体失败（已打开卷根关闭后返回错误，由调用方 fail-fast）。
// cfg 须已归一（Volumes 恒 ≥1，见 Default()/SetDefaults 契约）；空列表视为装配错误（fail-closed）。
func assembleVolumes(cfg *Config, log *slog.Logger) (*volumeSet, error) {
	log = defaultLogger(log)
	if len(cfg.Volumes) == 0 {
		return nil, fmt.Errorf("卷集合装配失败：volumes 为空（契约要求 Volumes 恒 ≥1）")
	}
	vs := &volumeSet{
		roots:   make(map[string]*storage.Root, len(cfg.Volumes)),
		pools:   make(map[string]*quota.Pool, len(cfg.Volumes)),
		tenants: make(map[string]*storage.Tenant),
	}
	for i := range cfg.Volumes {
		vc := cfg.Volumes[i]
		rootDir := vc.Root
		if i == 0 {
			rootDir = resolveDefaultVolumeRoot(cfg)
			// F1 诊断日志：首卷 root 为占位 defaultStorageRoot 被裁决覆写为 cfg.StorageRoot 时
			// 打 Warn——「配置写 ./storage、实际落 /data」可诊断（config 层 M-4 保留显式占位值，
			// 装配层视同未配，语义裂口见 resolveDefaultVolumeRoot 注释）。
			if rootDir != vc.Root {
				log.Warn("默认卷根 F1 裁决：Volumes[0].Root 为占位形态，改用 cfg.StorageRoot 建默认卷根",
					"volume", vc.Name, "placeholder_root", vc.Root, "storage_root", cfg.StorageRoot, "resolved_root", rootDir)
			}
		}
		if err := os.MkdirAll(rootDir, 0o755); err != nil {
			_ = vs.Close()
			return nil, fmt.Errorf("创建卷 %q 根目录失败（%s）: %w", vc.Name, rootDir, err)
		}
		rt, err := storage.OpenRoot(rootDir)
		if err != nil {
			_ = vs.Close()
			return nil, fmt.Errorf("打开卷 %q 根失败（%s）: %w", vc.Name, rootDir, err)
		}
		vs.roots[vc.Name] = rt
		vs.pools[vc.Name] = quota.NewPool(vc.VolCapacity)
		vs.volumes = append(vs.volumes, volume.Volume{
			Name:     vc.Name,
			RootDir:  rootDir,
			Capacity: vc.VolCapacity,
			ACL:      parseVolumeACL(vc.ACL),
		})
		if i == 0 {
			vs.defaultName = vc.Name
		}
		log.Info("卷装配完成", "volume", vc.Name, "root", rootDir, "capacity", vc.VolCapacity)
	}
	return vs, nil
}

// parseVolumeACL 把配置层 VolumeACLConfig 解析为 pkg/volume.ACL 纯域类型。
// 未配/缺省（nil）→ deny + 空名单 = 默认开放（AD-6 有意语义，兼容默认卷/旧单根）。
// 空 mode（防御，上游 SetDefaults 已归一 deny）→ deny。owners 逐项转 map（供 Authorize O(1)）。
func parseVolumeACL(ac *VolumeACLConfig) volume.ACL {
	acl := volume.ACL{Mode: volume.ModeDeny, Owners: map[string]struct{}{}}
	if ac == nil {
		return acl
	}
	acl.Mode = volume.Mode(ac.Mode)
	if acl.Mode == "" {
		acl.Mode = volume.ModeDeny
	}
	for _, o := range ac.Owners {
		acl.Owners[o] = struct{}{}
	}
	return acl
}

// ---- 写路径卷路由（任务 4）----

// routeErrorKind 区分 routeUpload 失败类别（换卷语义与 HTTP 映射用）。
type routeErrorKind int

const (
	routeErrOther routeErrorKind = iota
	routeErrNotAllowed
	routeErrConflict
	routeErrOwnerFull // owner 全局配额满：不换卷，直接 507
	routeErrVolFull   // 卷容量满：prefer-default 下换下一候选卷
)

// routeError 是 routeUpload 的失败载体：带 HTTP 状态与对外消息。
// kind 供自动路由换卷判断；err 为底层原因（如 quota.ErrStorageFull），errors.Is 可用。
type routeError struct {
	kind   routeErrorKind
	status int
	msg    string
	err    error
}

func (e *routeError) Error() string { return e.msg }
func (e *routeError) Unwrap() error { return e.err }

func newRouteError(kind routeErrorKind, status int, msg string, err error) *routeError {
	return &routeError{kind: kind, status: status, msg: msg, err: err}
}

// volumeRoute 是 routeUpload 的预留结果：目标卷 + 目标卷租户 + owner 全局 Scope 与卷容量池
// 双账本预留句柄。调用方写成功后 commit(prev, written)（覆盖写按 prev 双 Adjust 差分），
// 写失败 / 校验失败 / 幂等重复时 release() 双回滚。
type volumeRoute struct {
	volumeName string          // 空 = volSet 未装配（旧路径，无卷语义）
	tenant     *storage.Tenant // 目标卷上 owner 租户（写盘 root）
	scope      *quota.Scope    // owner 全局 Scope（globalPool 未装配时为 nil）
	scopeRes   *quota.Reservation
	pool       *quota.Pool // 目标卷容量池（volSet nil 时为 nil）
	poolRes    *quota.Reservation
}

// commit 双账本结算：新文件（prev==0）双 Commit(written)；覆盖写（prev>0）双 Adjust(prev,
// written) + 双 Release（预留全额归还，committed 只记尺寸差分——与既有单 Scope 覆盖写语义一致）。
func (r *volumeRoute) commit(prev, written int64) {
	if r.scopeRes != nil {
		if prev > 0 {
			r.scope.Adjust(prev, written)
			r.scopeRes.Release()
		} else {
			r.scopeRes.Commit(written)
		}
	}
	if r.poolRes != nil {
		if prev > 0 {
			r.pool.Adjust(prev, written)
			r.poolRes.Release()
		} else {
			r.poolRes.Commit(written)
		}
	}
}

// release 双回滚（写失败 / checksum 不匹配 / 幂等重复响应）。
func (r *volumeRoute) release() {
	if r.scopeRes != nil {
		r.scopeRes.Release()
	}
	if r.poolRes != nil {
		r.poolRes.Release()
	}
}

// routeUpload 为 owner 的 rel 选目标卷并在 owner 全局 + 卷容量双账本预留（写路径核心）。
// 语义（AD-7/§7，placement/ACL/换卷/显式唯一性/覆盖写 stay-home 全在此）：
//   - 候选 = AllowedVolumes(owner 视图)（ACL allow/deny 已解析进 volume.Volume.ACL）；
//   - 显式 volume（非空）→ 仅该卷候选且须在视图（不在 = 403）；显式先查唯一性（AD-4）：
//     目标卷 stat 已有同 rel → 409；视图其它卷已有同 rel → 409 + 所在卷名；
//   - forceHomeVol（非空）= 已存在 rel 的覆盖写 stay-home（F1-A/F1-B）→ 强制该 home 卷单候选：
//     容量路由只用于新文件，已存在 rel 必须写回其 home 卷；home 卷容量 TryReserve 满直接 507
//     不换卷（与单卷语义一致），防止跨卷双份 + owner 双计。home 卷不在 owner 视图 → 403。
//   - 自动路由（新文件，forceHomeVol 空）：候选按 cfg.Placement（OrderCandidates，used =
//     卷池 Usage 闭包）排序依序尝试：owner 全局 Scope TryReserve 成功 → 卷容量池 TryReserve；
//     卷池满 → Release owner 预留换下一候选；owner 全局满 → 直接 ErrStorageFull 不换卷；
//     全部卷满 → ErrStorageFull。
//
// 返回 volumeRoute（含双预留句柄）。volSet 未装配（旧装配路径）回落既有单卷行为：
// 默认租户 + owner 全局 Scope 预留，无卷容量池（零回归）。forceHomeVol 此时无卷语义
// （唯一根即 home），天然 stay-home。
func (h *Handlers) routeUpload(owner, rel, explicitVol string, size int64, forceHomeVol string) (*volumeRoute, error) {
	// 旧装配路径（volSet nil）：单卷零回归——默认租户 + owner 全局 Scope 预留。
	// forceHomeVol 此时无卷语义（唯一根即 home），天然 stay-home。
	if h.volSet == nil {
		tnt := h.tenantFor(owner)
		if tnt == nil {
			return nil, newRouteError(routeErrOther, http.StatusBadRequest, errMsgInvalidPath, nil)
		}
		route := &volumeRoute{tenant: tnt, scope: h.quotaScopeFor(owner, rel)}
		if route.scope != nil {
			res, err := route.scope.TryReserve(size)
			if err != nil {
				return nil, newRouteError(routeErrOwnerFull, http.StatusInsufficientStorage, "存储配额不足", err)
			}
			route.scopeRes = res
		}
		return route, nil
	}

	owner = normalizeOwner(owner)
	view := volume.AllowedVolumes(h.volSet.All(), owner)
	if len(view) == 0 {
		return nil, newRouteError(routeErrNotAllowed, http.StatusForbidden, "volume not allowed", nil)
	}

	// 覆盖写 stay-home（F1-A/F1-B）：rel 已在某卷命中（locateOwnerFile 定位的 home 卷）并走
	// 版本化覆盖写，强制回该 home 卷单候选，容量不足直接 507（reserveVolume 返回
	// routeErrVolFull/routeErrOwnerFull，上传侧映射），不换卷——防止同 rel 跨卷双份 + owner 双计。
	if forceHomeVol != "" {
		v, ok := h.volSet.ByName(forceHomeVol)
		if !ok || !v.Authorize(owner) {
			return nil, newRouteError(routeErrNotAllowed, http.StatusForbidden, "volume not allowed", nil)
		}
		return h.reserveVolume(owner, rel, v.Name, size)
	}

	// 显式指定卷：ACL 校验 + 唯一性查重，单候选双预留（无换卷）。
	if explicitVol != "" {
		v, ok := h.volSet.ByName(explicitVol)
		if !ok || !v.Authorize(owner) {
			return nil, newRouteError(routeErrNotAllowed, http.StatusForbidden, "volume not allowed", nil)
		}
		if err := h.checkVolumeUniqueness(owner, rel, v.Name, view); err != nil {
			return nil, err
		}
		route, err := h.reserveVolume(owner, rel, v.Name, size)
		if err != nil {
			return nil, err
		}
		return route, nil
	}

	// 自动路由：按 placement 排序候选，依序双预留；owner 全局满不换卷、卷满换下一候选。
	cfg := h.cfgPtr.Load()
	placement := volume.ModePreferDefault
	if cfg != nil && cfg.Placement != "" {
		placement = volume.Mode(cfg.Placement)
	}
	ordered := volume.OrderCandidates(view, placement, func(name string) int64 {
		if p := h.volSet.Pool(name); p != nil {
			return p.Usage()
		}
		return 0
	})
	var volFullErr error
	for _, v := range ordered {
		route, err := h.reserveVolume(owner, rel, v.Name, size)
		if err == nil {
			return route, nil
		}
		var re *routeError
		if errors.As(err, &re) && re.kind == routeErrVolFull {
			volFullErr = err // 卷容量满：保留最后一错误，继续下一候选
			continue
		}
		return nil, err // owner 全局满 / 其它：直接返回
	}
	if volFullErr != nil {
		return nil, volFullErr
	}
	return nil, newRouteError(routeErrVolFull, http.StatusInsufficientStorage, "存储配额不足", quota.ErrStorageFull)
}

// reserveVolume 对单卷做 owner 全局 Scope + 卷容量池双预留。
// owner 全局满 → routeErrOwnerFull（terminal）；卷池满 → routeErrVolFull（调用方可换卷）。
// 卷池预留失败时回滚 owner 全局预留。volSet.Tenant 取卷上租户（默认卷委托 h.tenantFor）。
func (h *Handlers) reserveVolume(owner, rel, volName string, size int64) (*volumeRoute, error) {
	tnt := h.volumeTenant(volName, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, newRouteError(routeErrOther, http.StatusBadRequest, errMsgInvalidPath, nil)
	}
	route := &volumeRoute{volumeName: volName, tenant: tnt, scope: h.quotaScopeFor(owner, rel)}
	if route.scope != nil {
		res, err := route.scope.TryReserve(size)
		if err != nil {
			return nil, newRouteError(routeErrOwnerFull, http.StatusInsufficientStorage, "存储配额不足", err)
		}
		route.scopeRes = res
	}
	if pool := h.volSet.Pool(volName); pool != nil {
		res, err := pool.TryReserve(size)
		if err != nil {
			if route.scopeRes != nil {
				route.scopeRes.Release()
			}
			return nil, newRouteError(routeErrVolFull, http.StatusInsufficientStorage, "存储配额不足", err)
		}
		route.pool, route.poolRes = pool, res
	}
	return route, nil
}

// volumeTenant 返回指定卷上 owner 的租户（写盘 root）。默认卷委托 h.tenantFor（既有
// tenantRoots 缓存，单卷零回归）；非默认卷走 volSet 懒建缓存（Tenant）。volSet nil / 未知
// 卷名回落默认租户语义（由调用方保证不会走到未知卷名）。
func (h *Handlers) volumeTenant(volName, owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if h.volSet == nil || volName == "" || volName == h.volSet.defaultName {
		return h.tenantFor(owner)
	}
	return h.volSet.Tenant(volName, owner, h.logger)
}

// volumeFileExists 探测指定卷上 owner 的 rel 是否已存在（只读，不创建租户目录）。
// 路径 = <卷根>/<owner>/<rel>（rel 含 user/ 前缀）。
// 语义 fail-closed：卷未知 → (false, nil)；stat 确证不存在（fs.ErrNotExist）→ (false, nil)；
// 其它 stat 错误（权限/IO 等）→ (false, err)，调用方按 500 处理——不得把「探测失败」当
// 「不存在」继续写（防权限/IO 错误下误覆盖判断）。
func (h *Handlers) volumeFileExists(volName, owner, rel string) (bool, error) {
	owner = normalizeOwner(owner)
	rt := h.volSet.Root(volName)
	if rt == nil {
		return false, nil
	}
	_, err := rt.Stat(owner + "/" + rel)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("探测卷 %q 文件状态失败: %w", volName, err)
}

// checkVolumeUniqueness 显式卷唯一性查重（AD-4）：目标卷已有同 rel → 409；
// 视图其它卷已有同 rel → 409 + 所在卷名。stat 探测失败（非不存在）→ 500 fail-closed。
// 仅在显式 volume 时调用（自动路由唯一性由路由保证）。
func (h *Handlers) checkVolumeUniqueness(owner, rel, targetVol string, view []volume.Volume) error {
	exists, err := h.volumeFileExists(targetVol, owner, rel)
	if err != nil {
		return newRouteError(routeErrOther, http.StatusInternalServerError, errMsgSaveFailed, err)
	}
	if exists {
		return newRouteError(routeErrConflict, http.StatusConflict,
			fmt.Sprintf("目标卷 %q 已存在同名文件", targetVol), nil)
	}
	for _, v := range view {
		if v.Name == targetVol {
			continue
		}
		exists, err := h.volumeFileExists(v.Name, owner, rel)
		if err != nil {
			return newRouteError(routeErrOther, http.StatusInternalServerError, errMsgSaveFailed, err)
		}
		if exists {
			return newRouteError(routeErrConflict, http.StatusConflict,
				fmt.Sprintf("同名文件已存在于卷 %q", v.Name), nil)
		}
	}
	return nil
}

// ---- 读路径跨卷定位（任务 5）----

// fileLocation 是一次跨卷定位结果：rel 所在卷名 + 目标卷上 owner 租户（读/删/改盘 root）。
// volumeName 空 = 无卷语义的旧装配路径（volSet nil，唯一根即默认租户）。
type fileLocation struct {
	volumeName string
	tenant     *storage.Tenant
}

// locateOwnerFile 返回 owner 卷视图内 rel（user/ 前缀完整桶路径）所在卷的租户（读定位）。
// 顺序：默认卷快路径（tenantFor 命中即用，覆盖绝大多数既有行为，单卷零回归）；
// 未命中才遍历视图其余卷（volumeFileExists 探测，不创建租户目录——只读无副作用）。
// 全部未命中返回 (nil, false)。
//
// 唯一性保证（AD-4）下同 rel 不会多卷命中；防御性若真出现多卷命中，默认卷优先 + 注释
// 注明「首卷胜出」——调用方只应依赖「至多一卷命中」的不变式。
//
// 返回不携带错误：探测失败的卷视同「未命中」（调用方通常回落默认卷根，由 Open/Stat 产出
// 404/500，与单卷既有错误语义一致）。显式指定卷的过滤定位见 locateForRead。
func (h *Handlers) locateOwnerFile(owner, rel string) (*fileLocation, bool) {
	if h.volSet == nil {
		// 旧装配路径：唯一根。stat 命中即定位成功（volumeName 空）。
		tnt := h.tenantFor(owner)
		if tnt == nil || tnt.Root() == nil {
			return nil, false
		}
		if _, err := tnt.Root().Stat(rel); err != nil {
			return nil, false
		}
		return &fileLocation{volumeName: "", tenant: tnt}, true
	}

	owner = normalizeOwner(owner)
	// 默认卷快路径：仅当默认卷在 owner 视图（ACL Authorize）内才可命中——tenantFor +
	// stat 命中即返回会跳过 AllowedVolumes 循环，默认卷被显式 allow 白名单收紧时未列入 owner
	// 经快路径仍能读到默认卷文件（ACL bypass，AD-6「所有定位/读取点先过 ACL」）。
	// 单卷缺省形态（deny + 空名单）Authorize 恒 true → 快路径行为不变（零回归）。
	if defVol, ok := h.volSet.ByName(h.volSet.defaultName); ok && defVol.Authorize(owner) {
		if defTnt := h.tenantFor(owner); defTnt != nil && defTnt.Root() != nil {
			if _, err := defTnt.Root().Stat(rel); err == nil {
				return &fileLocation{volumeName: h.volSet.defaultName, tenant: defTnt}, true
			}
		}
	}
	// 遍历视图其余卷（只探测，不创建租户目录）；默认卷不在视图则不会出现于 AllowedVolumes。
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		if v.Name == h.volSet.defaultName {
			continue
		}
		exists, err := h.volumeFileExists(v.Name, owner, rel)
		if err != nil || !exists {
			continue
		}
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil {
			continue
		}
		return &fileLocation{volumeName: v.Name, tenant: tnt}, true
	}
	return nil, false
}

// defaultVolumeAllows 判断 owner 是否被默认卷 ACL 放行（读/删/改名「未命中回落默认租户」前
// 的护栏——默认卷不在 owner 视图时不得回落默认租户 Open，防经回落读到默认卷自身遗留文件）。
// volSet nil（旧装配路径，无卷 ACL）→ 恒 true（唯一根即默认，零回归）。
func (h *Handlers) defaultVolumeAllows(owner string) bool {
	owner = normalizeOwner(owner)
	if h.volSet == nil {
		return true
	}
	v, ok := h.volSet.ByName(h.volSet.defaultName)
	return ok && v.Authorize(owner)
}

// primaryViewTenant 返回 owner 视图内首个卷的租户（写新目录/新文件等「不跨卷写」入口用）。
// 默认卷在视图时即默认租户（声明序首卷，单卷零回归）；默认卷被 ACL 排除时落到首个其它视图卷。
// 视图全空 / 卷租户不可用返回 nil（调用方按 400 fail-closed）。volSet nil（旧装配）回落默认租户。
func (h *Handlers) primaryViewTenant(owner string) *storage.Tenant {
	owner = normalizeOwner(owner)
	if h.volSet == nil {
		return h.tenantFor(owner)
	}
	for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
		tnt := h.volumeTenant(v.Name, owner)
		if tnt != nil && tnt.Root() != nil {
			return tnt
		}
	}
	return nil
}

// locateForRead 是读/删/改名路径的卷定位统一入口（带可选显式 volume 过滤）：
//   - explicitVol 非空 → 只在指定卷定位；未知卷名或 owner 不在该卷视图（ACL）→ 未命中
//     （fail-closed，调用方按 404，不泄卷存在性）；
//   - explicitVol 空 → 全视图定位（locateOwnerFile）。
func (h *Handlers) locateForRead(owner, rel, explicitVol string) (*fileLocation, bool) {
	owner = normalizeOwner(owner)
	if explicitVol != "" {
		if h.volSet == nil {
			return nil, false
		}
		v, ok := h.volSet.ByName(explicitVol)
		if !ok || !v.Authorize(owner) {
			return nil, false
		}
		exists, err := h.volumeFileExists(v.Name, owner, rel)
		if err != nil || !exists {
			return nil, false
		}
		tnt := h.volumeTenant(v.Name, owner)
		if tnt == nil {
			return nil, false
		}
		return &fileLocation{volumeName: v.Name, tenant: tnt}, true
	}
	return h.locateOwnerFile(owner, rel)
}
