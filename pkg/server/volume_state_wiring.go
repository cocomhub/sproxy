// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// volume_state_wiring.go —— 卷状态缓存的**唯一装配点**（设计
// docs/designs/2026-10-28-volume-state-maintenance.md §3.3）。
//
// 形态刻意与 `capacity.WrapStorageVolume`（R29 唯一入口）一致：横切关注点收敛到一处，
// 由门禁 R44 保证没有第二个包装点 ⇒ 新增卷型/新增装配路径自动获得该能力（P2）。
//
// **本期范围（用户裁定）**：只为**非本地卷**（外部/封装）套缓存。
//   - 外部卷今天「每次 list 直连远端、无任何缓存」= 最痛点；
//   - 本地卷仍走既有 `searchIndex`+`checksums.json`（R46 棘轮登记，只减不增，后续片迁移）。
// 因此本层对本地卷零行为变更。

// newVolumeStateCache 按配置构造缓存（未启用 ⇒ nil，零回归）。
func newVolumeStateCache(cfg *Config, vs *registry.Set, log *slog.Logger) *volumestate.Cache {
	if cfg == nil || !cfg.VolumeState.Enabled {
		return nil
	}
	log = slogutil.Default(log)
	store := volumestate.NewStore(cfg.StorageRoot, int64(cfg.VolumeState.MaxBytes), log)
	return volumestate.New(volumestate.Options{
		TTL: func(volName string) time.Duration {
			return volumeStateTTL(cfg, vs, volName)
		},
		Store: store,
		Persist: func(volName string) bool {
			return volumeStatePersist(cfg, vs, volName)
		},
		Log: log,
	})
}

// volumeStateTTL 解析某卷的缓存有效期：
//   - 配置显式给出（非 0）⇒ 全卷统一；
//   - 否则按卷类型：本地卷 0（不按时间过期，写穿即准）；外部卷 DefaultTTL（30s）。
func volumeStateTTL(cfg *Config, vs *registry.Set, volName string) time.Duration {
	if cfg.VolumeState.TTL != 0 {
		return cfg.VolumeState.TTL
	}
	if vs != nil && vs.HasLocalRoot(volName) {
		return 0
	}
	return volumestate.DefaultTTL
}

// volumeStatePersist 决定某卷的元数据快照是否允许落节点磁盘：
// 保密卷（加密/私密外部卷）默认**不落盘**（只内存），避免文件名/校验和明文落盘。
func volumeStatePersist(cfg *Config, vs *registry.Set, volName string) bool {
	if cfg.VolumeState.PersistPrivate {
		return true
	}
	if vs == nil {
		return true
	}
	v, ok := vs.ByName(volName)
	if !ok {
		return true // 元信息缺失（极罕见装配异常）：不因缺信息降低可用性
	}
	return !volumePrivate(v)
}

// wrapVolumeState 在卷视图装饰链**最外层**套状态缓存（仅 ViewUser + 非本地卷）。
//
// 为何最外层：读命中应短路在 trusted/capacity 之前（省掉 meta 读与计数）；写失效也应
// 在内层全部成功之后才发（装饰器委派成功后才调用 Invalidate）。
// 本地卷本期不套（仍走既有 searchIndex；R46 棘轮登记，后续片迁移）。
func wrapVolumeState(vs *registry.Set, cache *volumestate.Cache, name string, use registry.ViewUse, fsys syncpkg.FS) syncpkg.FS {
	if cache == nil || fsys == nil || use != registry.ViewUser {
		return fsys
	}
	if vs != nil && vs.HasLocalRoot(name) {
		return fsys
	}
	return volumestate.Wrap(name, fsys, cache)
}
