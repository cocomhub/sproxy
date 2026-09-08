// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes.go 实现多卷存储的 server 侧装配（任务 3）：把 cfg.Volumes 逐卷打开根 + 建卷容量池
// + ACL 解析为 pkg/volume 纯域类型，并处理 F1 缺省卷根裁决（storage_root 与占位 volumes 的解耦）。
//
// 默认卷 = cfg.Volumes[0]；globalRoot/tenantFor 等既有 handler 语义映射到默认卷根，本任务不把
// 写路径切到卷感知（T4 起），因此单卷零回归可测。

import (
	"fmt"
	"log/slog"
	"os"

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
}

// Default 返回默认卷描述（volumes[0]）。
func (vs *volumeSet) Default() volume.Volume {
	return volume.DefaultVolume(vs.volumes)
}

// All 返回全部装配卷（声明序，默认卷在 [0]）。
func (vs *volumeSet) All() []volume.Volume {
	return vs.volumes
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
	for name, rt := range vs.roots {
		if rt != nil {
			_ = rt.Close()
		}
		delete(vs.roots, name)
	}
	return nil
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
		roots: make(map[string]*storage.Root, len(cfg.Volumes)),
		pools: make(map[string]*quota.Pool, len(cfg.Volumes)),
	}
	for i := range cfg.Volumes {
		vc := cfg.Volumes[i]
		rootDir := vc.Root
		if i == 0 {
			rootDir = resolveDefaultVolumeRoot(cfg)
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
