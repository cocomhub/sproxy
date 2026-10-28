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
// **范围（2026-10-28 决策，设计 §15.3）**：缓存**覆盖所有存储卷（含本地卷）**，但只在
// 默认生效（`volume_state.disable: true` 才关闭）。
//   - 外部/封装卷：list 原本直连远端、无缓存 = 最痛点；
//   - **本地卷**：为 §15.2 步骤 2（list 改走卷状态视图）提供性能前提 —— 否则每次列表都是
//     O(目录项) 磁盘扫描。本地卷的新鲜度契约 = **写穿（Root 事件，含内容写）+ TTL 兜底**：
//     进程外改动（运维直写盘/同根多实例）≤TTL 后可见（此前 `searchIndex` 同类盲区无收敛承诺）。
//   - 本地卷的对外 list 目前仍走 `searchIndex`（R46 棘轮收敛中）；本层先让 `FS.ListDir` 消费方
//     （同步/镜像/备份/校验）享受缓存与事件失效。

// newVolumeStateCache 按配置构造缓存（未启用 ⇒ nil，零回归）。
func newVolumeStateCache(cfg *Config, vs *registry.Set, log *slog.Logger) *volumestate.Cache {
	if cfg == nil || cfg.VolumeState.Disable {
		return nil
	}
	log = slogutil.Default(log)
	store := volumestate.NewStore(cfg.StorageRoot, int64(cfg.VolumeState.MaxBytes), log)
	return volumestate.New(volumestate.Options{
		TTL: func(volName string) time.Duration {
			return volumeStateTTL(cfg, volName)
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
//   - 否则一律 DefaultTTL（5 分钟：缓存是正常路径；进程外改动属罕见场景）。
//
// **为什么本地卷也必须有 TTL**（设计 §15.3）：本地卷的写穿只覆盖**本进程**的 Root 变更；
// 进程外改动（运维 rsync/备份恢复直接写盘、同根多实例）没有事件可观察，TTL=0 意味着永不回源
// ⇒ 这些改动永远不可见。`rootfs` 的目录 mtime 代际也不能替代 TTL——POSIX 只更新**直接父目录**
// 的 mtime，祖先探测发现不了深层变化。
func volumeStateTTL(cfg *Config, volName string) time.Duration {
	_ = volName // 保留按卷覆盖的扩展点（当前全卷统一）
	if cfg.VolumeState.TTL != 0 {
		return cfg.VolumeState.TTL
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

// wrapVolumeState 在卷视图装饰链**最外层**套状态缓存（ViewUser；所有存储卷，含本地卷）。
//
// 为何最外层：读命中应短路在 trusted/capacity 之前（省掉 meta 读与计数）；写失效也应
// 在内层全部成功之后才发（装饰器委派成功后才调用 Invalidate）。
//
// 本地卷的失效来源 = `storage.Root` 事件（结构 + 内容写，见 volume_mutation_wiring.go），
// 因此写穿是精确的；TTL 只用于吸收进程外改动（见 volumeStateTTL 注释）。
func wrapVolumeState(cache *volumestate.Cache, name string, use registry.ViewUse, fsys syncpkg.FS) syncpkg.FS {
	if cache == nil || fsys == nil || use != registry.ViewUser {
		return fsys
	}
	return volumestate.Wrap(name, fsys, cache)
}
