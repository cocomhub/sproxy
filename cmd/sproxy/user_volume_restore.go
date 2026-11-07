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
	serverenclosure "github.com/cocomhub/sproxy/pkg/server/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
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
		ACL:      uv.ACL, // 保留持久化 ACL（新卷 owner-only；已存卷零值 = 开放，兼容不收紧）
	}
}

// restoreUserVolumes 扫描全部 owner 的用户卷并恢复进 set（运行时外部卷注册）。单卷失败
// 跳过 + 告警；store 扫描本身失败 → 返回错误（装配 fail-closed）。
//
// **依赖排序（阶段 2 迁移后仍需要）**：嵌套封装卷（enclosure）的装配需要其源卷已登记进卷集
// （`enclosure.SourceFS` 取源特权视图）。原实现依赖 ScanRestore 的 (owner,name) 序，源卷名序在
// 封装卷之后时装配早失败。故按依赖分轮：每轮只恢复「源卷已就绪」的卷，无进展轮对剩余卷逐卷
// 落旧行为（单卷跳过 + 告警，避免死循环）。
func restoreUserVolumes(set *registry.Set, store *server.UserVolumeStore, log *slog.Logger) error {
	if set == nil || store == nil {
		return nil // 防御：未装配卷集合/用户卷 store 时不恢复
	}
	uvs, err := store.ScanRestore()
	if err != nil {
		return err
	}
	// 存量卷 ACL 可观测性（2026-10-15 代码审核 A 轮发现）：S1 的「用户卷强制 owner-only ACL」
	// 只在**创建/恢复时注入**；**旧 store 文件无 acl 字段** ⇒ 读回零值 ACL（Mode=="" =
	// 默认开放）⇒ 该卷对**任意 owner** 可枚举/可写，而此前**无任何提示**（静默 ✗）。
	warnOpenACLUserVolumes(uvs, log)
	pending := uvs
	for {
		var next []server.UserVolume
		progress := restoreReadyRound(set, store, log, pending, &next)
		if len(next) == 0 {
			return nil
		}
		if !progress {
			// 无进展：剩余卷源卷不可恢复（被跳过/缺失）→ 逐个落位（单卷失败即告警跳过），
			// 避免死循环（兼容旧行为，装配容忍单盘坏）。
			for _, uv := range next {
				restoreOneUserVolume(set, store, log, uv)
			}
			return nil
		}
		pending = next
	}
}

// restoreReadyRound 处理一轮依赖恢复：把「源卷已就绪」的卷恢复进 set，源卷未就绪（嵌套依赖）
// 的卷放入 next 待下轮。返回本轮是否有进展（任一卷被处理）。
func restoreReadyRound(set *registry.Set, store *server.UserVolumeStore, log *slog.Logger, pending []server.UserVolume, next *[]server.UserVolume) bool {
	progress := false
	for _, uv := range pending {
		base, _, nested := splitNestedUVTarget(uv)
		if nested && !setVolumeReady(set, base) {
			*next = append(*next, uv) // 源卷未就绪 → 暂缓本轮（依赖后置）。
			continue
		}
		restoreOneUserVolume(set, store, log, uv)
		progress = true
	}
	return progress
}

// warnOpenACLUserVolumes 汇总告警「ACL 未收紧（默认开放）」的存量用户卷。
//
// 判据与 `volume.Authorize` 同源：非 `ModeAllow`（零值/ModeDeny）即「默认开放 + 黑名单」语义
// ⇒ 对任意 owner 放行；即 ModeAllow 但白名单为空也同样无有效授权（属配置异常）。
// 单条聚合日志（数量 + 前若干个卷名），避免多卷部署启动刷屏。
func warnOpenACLUserVolumes(uvs []server.UserVolume, log *slog.Logger) {
	if log == nil || len(uvs) == 0 {
		return
	}
	var open []string
	for _, uv := range uvs {
		if uv.ACL.Mode == volume.ModeAllow && len(uv.ACL.Owners) > 0 {
			continue // 已按 owner-only 收紧（新建/恢复路径注入）
		}
		open = append(open, uv.Owner+"/"+uv.Name)
	}
	if len(open) == 0 {
		return
	}
	sample := open
	if len(sample) > 5 {
		sample = sample[:5]
	}
	log.Warn("存量用户卷 ACL 未收紧（默认开放：任意 owner 可枚举/写）——建议显式配置 ACL 或重建为 owner-only",
		"count", len(open), "sample", sample)
}

// setVolumeReady 判定源卷 base 已装配进 set（封装卷装配依赖其存在）。
func setVolumeReady(set *registry.Set, base string) bool {
	_, ok := set.ByName(base)
	return ok
}

// restoreOneUserVolume 恢复单卷：**嵌套封装卷**（用户面 target 语法）经装配层翻译出封装声明
// 词汇后走 `serverenclosure.AttachEnclosureVolume`（声明校验 + 封印注入 + 容量池继承 + 后端构造一体；
// 卷描述 Type 保持 wrapper 类型），其余类型维持 `registry.NewBackend` + CapacityFS 计数器包装。
//
// 翻译与建卷路径共用 `serverenclosure.TranslateNestedVolume`（单一实现）；持久化仍是原形态
// （store 字节零变化：同一份 store 在迁移前后都能恢复）。返回是否恢复成功（false = 单卷跳过，
// 不阻塞其余卷）。
func restoreOneUserVolume(set *registry.Set, store *server.UserVolumeStore, log *slog.Logger, uv server.UserVolume) bool {
	// 存量记录显式拒绝（决策④，2026-10-15）：`type: enclosure` 自 f97e9346a 起不是可写卷类型，
	// 且**历史实现也从不持久化该 type**（翻译层只在内存改写 Type，store 恒存用户面原形态
	// `wrapper 类型 + extra.target`；建卷 API 对该 type 在落盘前即失败）。故正常 store 不会出现
	// 该记录——若出现（人工编辑/外部工具写入），按「明确拒绝并告警」处理，绝不猜测其 wrapper。
	if uv.Type == enclosure.TypeEnclosure {
		log.Warn("用户卷恢复拒绝：type=enclosure 是内部装配机制，不是可持久化卷类型"+
			"（请把该记录改写为 wrapper 类型 + extra.source）", "volume", uv.Name, "owner", uv.Owner)
		return false
	}
	v := volumeFromUserVolume(uv)
	ctx := context.Background()
	tv, nested := serverenclosure.TranslateNestedVolume(v)
	if nested {
		// 封装卷的容量计量经**继承池**（父链聚合到源卷池）+ 持久化计数器（AttachEnclosureVolume
		// 内包 CapacityFS），与本进程内建卷路径同源 ⇒ 重启前后额度口径一致。
		if err := serverenclosure.AttachEnclosureVolume(ctx, set, log, store.Root(), tv); err != nil {
			log.Warn("用户卷恢复跳过（封装卷装配失败）", "volume", uv.Name, "owner", uv.Owner, "error", err)
			return false
		}
		log.Info("用户卷已恢复", "volume", uv.Name, "owner", uv.Owner, "type", uv.Type, "capacity", uv.Capacity, "enclosure", true)
		return true
	}
	be, bErr := registry.NewBackend(ctx, v)
	if bErr != nil {
		log.Warn("用户卷恢复跳过（后端构造失败）", "volume", uv.Name, "owner", uv.Owner, "error", bErr)
		return false
	}
	// C2 卷级计数：包 CapacityFS。计数器是**唯一实现**（分片日志账本，建在**原始**卷 FS 上
	// ⇒ 不自记账、跨进程正确、重启从已占用起算）。限额 = 元数据里的**生效上限**
	// （创建/重建时经 `capacity.LoadVolumeLimit` 处理 F6 继承后落库）。
	statePath := capacity.VolumeLimitPath(store.Root(), uv.Owner, uv.Name)
	baselineUsed := capacity.LoadVolumeUsed(statePath)
	// 账本落点：secretdata（加密卷）按**底层本地性**（探测在 WrapStorageVolume 内部）——
	// 本地 ⇒ 落**节点本地**明文目录（写卷 FS 上会被加密成随机内容分块，污染卷物理布局）；
	// 共享 ⇒ 账本留卷 FS（跨进程正确，配额不被绕过）。与建卷路径同口径。
	opts := []capacity.WrapOption{}
	if uv.Type == volume.TypeSecretdata {
		opts = append(opts, capacity.WithLedgerDirIfLocal(
			filepath.Join(store.Root(), uv.Owner, "meta", "quota", uv.Name)))
	}
	be = capacity.WrapStorageVolume(uv.Name, be, nil, nil, uv.Capacity, baselineUsed, opts...)
	if lerr := capacity.WriteVolumeState(statePath, uv.Capacity, baselineUsed); lerr != nil {
		log.Warn("用户卷状态持久化失败（重建将无法继承上限/用量）", "volume", uv.Name, "owner", uv.Owner, "error", lerr)
	}
	if aErr := set.AddExternalVolume(v, be); aErr != nil {
		log.Warn("用户卷恢复跳过（注册失败）", "volume", uv.Name, "owner", uv.Owner, "error", aErr)
		_ = be.Close()
		return false
	}
	log.Info("用户卷已恢复", "volume", uv.Name, "owner", uv.Owner, "type", uv.Type, "capacity", uv.Capacity, "enclosure", false)
	return true
}

// splitNestedUVTarget 从 UserVolume.Extra 解析嵌套封装 target（`<卷>/<子目录>`）的**源卷段**
// （依赖排序只需源卷名）。非嵌套 / 无 target → ok=false。
func splitNestedUVTarget(uv server.UserVolume) (base, subdir string, ok bool) {
	target, _ := uv.Extra["target"].(string)
	return serverenclosure.ParseNestedTarget(target)
}
