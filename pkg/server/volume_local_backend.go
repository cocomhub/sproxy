// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// volume_local_backend.go —— 本地卷的**统一后端实现**（统一存储卷模型 M1）。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。
//
// 现状（本文件要消除的特例）：本地卷**不是**注册后端 —— 装配层以 `Type == "" || TypeLocal`
// 走 `assembleLocalVolume` 特例分支，`*storage.Root` 因此泄漏到非测试源码 **254 处**；
// 而外部卷早已统一（`registry.ExternalBackend` = `FS() + Close()`）。
//
// 本片（M1a）只提供**实现**，**不注册**（`registry.RegisterBackend(volume.TypeLocal, …)` 由
// 下一片连同 2 个 API 面守卫一起做）⇒ **零行为变更** ✓：
//   - `NewBackend` 自身仍守卫 local（`registry/backend.go`）⇒ 无调用点行为改变 ✓；
//   - `BackendSchemas`/`IsBackendRegistered` 仍不含 local ⇒ backend 列表 API 不变 ✓。
//
// **为什么不做第二份实现**：`OpenLocalVolumeRoot`（MkdirAll + `storage.OpenRoot` + at-rest
// 加密 key 解析/cipher 校验，fail-closed）**已是**本地卷根开启的唯一实现 ⇒ 本后端**复用它** ✓
// （禁复制加密/布局逻辑 ✗）；`NewRootFS` 同理（卷根 → `sync.FS` 的唯一适配器 ✓）。
//
// **命名注记**（用户裁定 2026-10-16）：`ExternalBackend` 是**过时命名** —— 整合完成后允许
// **破坏性变更**统一命名（`ExternalBackend` → `StorageVolume` 等），届时不得为兼容而保留双名 ✗。
type localBackend struct {
	name      string
	root      *storage.Root
	fs        syncpkg.FS
	encrypted bool // at-rest 加密（`extra.encrypt`）⇒ 物理足迹 = 密文长度、上界 = 公式折算
}

// NewLocalBackend 构造本地卷后端：复用 `OpenLocalVolumeRoot`（含 at-rest 加密与 fail-closed
// 语义）⇒ 与装配层**同一实现**。失败时已关闭本根（`OpenLocalVolumeRoot` 保证 ✓）。
func NewLocalBackend(v volume.Volume, rootDir string, log *slog.Logger) (*localBackend, error) {
	if log == nil {
		log = slog.Default()
	}
	root, err := OpenLocalVolumeRoot(v.Name, rootDir, v.Extra, log)
	if err != nil {
		return nil, err
	}
	return &localBackend{
		name:      v.Name,
		root:      root,
		fs:        NewRootFS(root),
		encrypted: vcExtraBool(v.Extra, "encrypt"),
	}, nil
}

// Name 返回卷名（装配期唯一）。
func (b *localBackend) Name() string { return b.name }

// Kind 返回结构性类型：本地卷**承载真实字节** ⇒ `KindStorage`（需配额层 ✓）。
func (b *localBackend) Kind() volume.Kind { return volume.KindStorage }

// FS 返回卷根相对键空间的数据面视图（装配后只读：同一实例）。
func (b *localBackend) FS() syncpkg.FS { return b.fs }

// Close 关闭卷根（幂等由 `storage.Root.Close` 保证）。
func (b *localBackend) Close() error { return b.root.Close() }

// Root 暴露底层卷根（**过渡期**：装配层/路由仍以 `*storage.Root` 访问；M3 收敛后删除 ✗）。
func (b *localBackend) Root() *storage.Root { return b.root }

// ---- 可选能力：物理足迹 / 写前上界（设计 §9 片 2）----
//
// 为何由**卷**自报（而非调用方折算）：配额层与真实字节之间**没有变换层** ⇒ 它必须拿到**物理**
// 字节；而「逻辑→物理」的折算规则**只属于卷**（加密 = 公式；外部 = 远端 stat；secretdata = 只能
// 实测）⇒ 公式**只写在卷里** ✓（本会话 4 例「单位漂移」缺陷的根因就是折算义务落在了调用方 ✗）。
//
// 本卷两种形态：
//   - **明文**：物理 == 逻辑（`Stat` 即真值 ✓）；
//   - **at-rest 加密**：`storage.Root` 内部透明加解密 ⇒ `Stat` 返回**密文长度** ✓（天然物理 ✓）、
//     写前上界 = `storage.EncryptedPhysicalSize`（与落盘真值同公式 ✓）。

// footprintKey 把用户可见路径 (owner, rel) 映射为卷内键（`Location.FSPath()` 同口径：
// `<归一 owner>/<rel>` ✓）。rel 已含功能桶前缀（如 `user/dir/f.txt` ✓）。
func footprintKey(owner, rel string) string {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return storage.NormalizeOwner(owner)
	}
	return storage.NormalizeOwner(owner) + "/" + rel
}

// Footprint 返回 (owner, rel) 对象在本卷上的**物理足迹**（磁盘真值 ✓）。
//
// 语义：`ok=true` 表示**测得**（`n` 可为 0：目录在明文本地卷上不占记账字节 ✓）；
// `ok=false` 表示**不可得**（不存在 / stat 失败）⇒ 调用方必须显式处置（禁静默当 0 ✗）。
func (b *localBackend) Footprint(owner, rel string) (int64, bool) {
	if b == nil || b.root == nil {
		return 0, false
	}
	fi, err := b.root.Stat(footprintKey(owner, rel))
	if err != nil || fi == nil {
		return 0, false
	}
	if fi.IsDir() {
		return 0, true // 目录：明文/加密本地卷均无 meta blob ⇒ 0 是**实测值**而非未知
	}
	return fi.Size(), true
}

// MaxPhysical 返回 logical 字节在本卷上的**记账上界**（写前预占，严格 ≥ 实际占用）。
//
// 加密卷必须用 `storage.EncryptedPhysicalSize`（与落盘公式同源 ✓）——若恒等返回 logical，
// 预占会**小于**真实密文长度 ⇒ 写后结算超出已预留额度 ⇒ G1「绝不超真实配额」失效 ✗。
func (b *localBackend) MaxPhysical(owner, rel string, logical int64) int64 {
	if logical <= 0 {
		return logical
	}
	if b != nil && b.encrypted {
		return storage.EncryptedPhysicalSize(logical)
	}
	return logical
}

// 编译期：本地卷后端**同时**满足外部后端契约与统一存储卷契约 —— 这是「`local` 可作为普通
// 后端注册」的前提（M1 注册片 + M3 迁移片都依赖它），并实现配额层所需的足迹/上界能力（片 2）。
var (
	_ registry.ExternalBackend = (*localBackend)(nil)
	_ registry.StorageVolume   = (*localBackend)(nil)
	_ volume.Footprinter       = (*localBackend)(nil)
	_ volume.PhysBound         = (*localBackend)(nil)
)

// registerLocalBackendOnce 保证注册幂等（`RegisterBackend` 重复注册即 panic ⇒ 多次装配
// （生产 / 测试）不得重复调用）。
var registerLocalBackendOnce sync.Once

// RegisterLocalBackend 把本地卷注册为**普通后端**（M1b）：`registry.NewBackend` 从此可统一
// 分派本地卷，装配层特例分支（`Type == "" || TypeLocal`）可在后续分片删除（M5）。
//
// **不声明任何协议 scheme**：本地卷不经 URL 寻址（`scheme://卷/路径` 是外部/封装卷语义 ✓）。
// 根目录取自 `v.RootDir`（配置层 `volumes[].root` ✓）；加密/布局逻辑复用
// `OpenLocalVolumeRoot`（见 `NewLocalBackend` ✓）。
//
// **注册 ≠ 用户可创建**：建卷 API / 创建表单 / 预签名面另由 `IsUserCreatableBackendType`
// 排除本地 ✓（否则可指定任意 `RootDir` ⇒ 路径/权限面 ✗）。
func RegisterLocalBackend() {
	registerLocalBackendOnce.Do(func() {
		// U4：本地卷声明 `local://` 协议（转存 URL 一致化——`local://<卷>/<rel>`）。
		// 与既有 `volume://`（卷内定位序列化）分工：`local://` 是协议寻址（转存 URL），
		// `volume://` 是 `Location.String()` 的卷内定位形态。
		registry.RegisterBackend(volume.TypeLocal, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return NewLocalBackend(v, v.RootDir, slog.Default())
		}, "local")
	})
}
