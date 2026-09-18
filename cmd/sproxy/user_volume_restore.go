// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// user_volume_restore.go 是用户卷重启恢复装配（U4）：装配层扫描 `<storage_root>/<owner>/meta/volume/`
// 恢复用户卷到 registry.Set（运行时外部卷），供同步任务工厂查询。
//
// 与 baidupcs 系统盘（volumes[] type=baidupcs，装配期静态）互补：用户卷是 per-owner 动态卷
// （U3 管理 API 创建），重启后需经 store.ScanRestore 恢复进 Set.external——否则同步任务
// 工厂查 Set.External(volume) 会 miss 已持久化的用户卷。
//
// 单卷恢复失败（后端构造错误/凭据失效）→ 该卷跳过 + 告警，其余卷继续恢复（仿 baidupcs
// 装配容忍单盘坏）；不整体失败。

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volumeFromUserVolume 把 UserVolume（store 持久化模型）转换为 volume.Volume（registry 消费）。
// Owner 不落入 volume.Volume（Set 无 owner 字段）；owner 归属校验由装配层 SetUserVolumeOwner
// 闭包查 store（Get(owner,name)）完成。
func volumeFromUserVolume(uv server.UserVolume) volume.Volume {
	return volume.Volume{
		Name:     uv.Name,
		Type:     uv.Type,
		Capacity: uv.Capacity,
		Extra:    uv.Extra,
	}
}

// restoreUserVolumes 扫描全部 owner 的用户卷并恢复进 set（运行时外部卷注册）。
// 单卷失败跳过 + 告警；store 扫描本身失败 → 返回错误（装配 fail-closed）。
func restoreUserVolumes(set *registry.Set, store *server.UserVolumeStore, log *slog.Logger) error {
	if set == nil || store == nil {
		return nil // 防御：未装配卷集合/用户卷 store 时不恢复
	}
	uvs, err := store.ScanRestore()
	if err != nil {
		return err
	}
	for i := range uvs {
		uv := uvs[i]
		v := volumeFromUserVolume(uv)
		be, bErr := registry.NewBackend(context.Background(), v)
		if bErr != nil {
			log.Warn("用户卷恢复跳过（后端构造失败）", "volume", uv.Name, "owner", uv.Owner, "error", bErr)
			continue
		}
		// C2 卷级计数：包 CapacityFS（counter 持久化 <root>/<owner>/meta/volume/<name>.capacity.json）。
		// 限额 = UserVolume.Capacity（本系统可用该卷多少；0 = 不限）。
		counterPath := filepath.Join(store.Root(), uv.Owner, "meta", "volume", uv.Name+".capacity.json")
		cnt, cErr := capacity.Load(counterPath, uv.Capacity)
		if cErr != nil {
			log.Warn("用户卷容量恢复失败（按新计数器）", "volume", uv.Name, "owner", uv.Owner, "error", cErr)
			cnt = capacity.NewCounter(uv.Capacity, counterPath)
		}
		be = wrapBackendWithCapacity(be, cnt)
		if aErr := set.AddExternalVolume(v, be); aErr != nil {
			log.Warn("用户卷恢复跳过（注册失败）", "volume", uv.Name, "owner", uv.Owner, "error", aErr)
			_ = be.Close()
			continue
		}
		log.Info("用户卷已恢复", "volume", uv.Name, "owner", uv.Owner, "type", uv.Type, "capacity", uv.Capacity)
	}
	return nil
}

// wrapBackendWithCapacity 把 backend 的 FS 包为 CapacityFS（卷级计数记账）。
func wrapBackendWithCapacity(be registry.ExternalBackend, cnt *capacity.VolumeCapacityCounter) registry.ExternalBackend {
	return &capacityBackend{ExternalBackend: be, fs: capacity.Wrap(be.FS(), cnt), counter: cnt}
}

// capacityBackend 是包装后的 backend：FS 返回 CapacityFS（记账），Usage 暴露已用字节。
type capacityBackend struct {
	registry.ExternalBackend
	fs      *capacity.CapacityFS
	counter *capacity.VolumeCapacityCounter
}

func (b *capacityBackend) FS() syncpkg.FS { return b.fs }

// Usage 返回本系统已占用该卷的字节（C3 查询 API 用）。
func (b *capacityBackend) Usage() int64 { return b.counter.Used() }

// Capacity 返回本系统可用限额。
func (b *capacityBackend) Capacity() int64 { return b.counter.Capacity() }
