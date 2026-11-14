<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一存储卷模型：StorageVolume 唯一身份 + VolumeView 唯一取卷入口 — 归档

> **归档说明**：原文 `docs/designs/2026-10-28-unified-volume-model.md` 已于 2026-11-14 归档。
> **U1–U5 已全部落地**；现行事实源见 `docs/architecture.md`「统一存储卷模型（StorageVolume 唯一
> 身份 + VolumeView 唯一取卷入口）」与 `AGENTS.md` 能力矩阵（`pkg/volume` + `registry` 行）。
> 本文件只保留**背景痛点 / 目标架构 / 安全性取舍 / 关键不动边界（避免过度设计）/ 落地指针**。

## 背景（用户 2026-10-28 明示）
「目前本地卷是特例，无法使用 Volume 通用卷接口（例如转存时无法选择本地卷）；项目维护了本地卷
和外部卷两大类卷，长期维护复杂，新功能要实现两份。设计方案统一成一套接口降低维护复杂度。
注意**必须按卷操作文件，避免直接用裸 FS 跳过各种卷功能封装**；目标是根治方案统一架构，
避免小修小补治标不治本。」

前置：`docs/designs/2026-10-16-quota-volume-layer.md` §11（M0-M5 统一存储卷模型的既有分片）——
本设计是其**具体化续篇**。原文取证发现的关键形态：`localBackend`（M1a）+ `RegisterLocalBackend`
（M1b）**已存在但装配未接线**——「注册了却没接上」正是「半成品根治」的形态；本设计把它接完。

## 目标架构（六层收敛）
**核心原则**：`StorageVolume` 是**唯一卷身份**；`Set.VolumeView(name, use)` 是**唯一取卷入口**
（返回按用途封装的 FS，装配期已套完整装饰链）；消费方**永远拿不到裸 FS**（根治 R27 类直连点）。
- `assembleLocalVolume` 改走 `NewLocalBackend`（`*storage.Root` 成为本地卷内部细节，仅默认卷
  tenant/globalRoot 存量路径保留）；
- **按用途封装**：同一卷按用途返回不同装饰链视图——`user`（用户文件：Guard → trusted.Wrap →
  capacity → staging）、`meta`（控制面：Guard，不经容量）、`secrets`（密钥，唯一绕行点，
  **不入通用表**）、任务桶（cloud/archive/chunk/version）；外部卷同规则。
- 装饰链在**装配期**经 `SetVolumeViewDecorator` 注入（registry 内置默认 trusted 装饰，测试/生产
  行为一致）。

## 安全性取舍
| 面 | 风险 | 对策 |
|---|---|---|
| `VolumeView(name, "secrets")` | 泄露密钥视图 | secrets 视图**不入通用表**，只在装配期按需构造（R26 白名单维持）；通用 `VolumeView` **拒绝** `secrets` use |
| `VolumeView(name, "meta")` | 绕 meta 桶隔离 | meta 视图仅控制面消费（sidecar/checksum/台账），用户读路径不得用 |
| 本地卷 trusted 启用 | 老文件无 sidecar | 读侧**跳过**校验（不失败），写侧生成——与外部卷既有语义一致 |
| `local://` URL 解析 | 路径穿越 | `ResolveURL` 经 `volume.ResolveUserLocation`（权限门 + 路径安全）——与读路径同入口 |
| 装配接线 | 本地卷容量记账漏接 | `assembleLocalVolume` 走 `NewLocalBackend` 后**同样套 capacity**，确保本地卷用户写入也计数 |

## 关键不动边界（避免过度设计，仍有效）
1. **本地卷写内核仍用 `Root.OpenFileEncrypted` + `AtomicRename`**——原子写/加密语义只在 Root 上
   完备；统一的是**身份与访问入口**，不是把 Root 32 方法塞进接口。
2. **默认卷 `globalRoot`/`tenantFor` 保持 `storage.Root`**（租户布局语义）。
3. **`volume://` 与 `local://` 并存**：`volume://` 是卷内定位序列化（`Location.String()`，已存在）；
   `local://` 是协议寻址（转存 URL）。
4. **trusted 装饰对本地卷启用**（与外部卷一致）。
5. **不做**：不改 `volume://` 序列化；不在本次迁移 `pkg/files` 写内核（U5 只收缩访问器调用点）。

## 落地结果 / 指针
- **U5 门禁预算只降不升**：带 `ListDir` 的 FS 实现、`Set.Root`/裸 `be.FS()` 直连点收缩
  （能迁的迁到 `VolumeView`）；确需 Root 的（写内核/封印/ListOwners）逐条登记理由（R28/R27）。
- **转存支持本地卷**：`checkTransferVolumePreflight` 只要求目标卷可解析（FS 非 nil），产物落
  `<owner>/user/<rel>`，可经 `/download` 命中；本地卷转存的 owner 配额经 `rootfs` 自述
  `syncpkg.LocalVolume` 生效；卷级容量池经装配注入做写前探测 + 写后差分结算。
- 落地指针：`pkg/volume/registry`（`set.go` 的 `StorageVolume`/`VolumeView`/`views.go`）、
  `pkg/server/volume_local_backend.go`、`pkg/server/volumes.go`、`cmd/sproxy/root.go`。
