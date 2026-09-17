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

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/volume"
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
		if aErr := set.AddExternalVolume(v, be); aErr != nil {
			log.Warn("用户卷恢复跳过（注册失败）", "volume", uv.Name, "owner", uv.Owner, "error", aErr)
			_ = be.Close()
			continue
		}
		log.Info("用户卷已恢复", "volume", uv.Name, "owner", uv.Owner, "type", uv.Type)
	}
	return nil
}
