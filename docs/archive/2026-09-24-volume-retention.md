<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 卷级数据保留策略（`volumes[].retention`）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-volume-retention.md` 已于 2026-11-13 归档。
> 配置字段以 `docs/config.md`（`volumes[].retention.*`）为准（现行事实源），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
版本保留（`versioning.retention`）、分享过期（`expire_at`）、审计轮转三套 TTL 各自为政，
且都是「全局/单点」语义。目标是给卷一个统一的保留策略入口（版本龄、分享兜底、审计龄、GC 周期）。

## 定案与取舍（仍有效）
1. **卷级优先、全局兜底**：卷配置 `version_ttl > 0` 时覆盖全局 `versioning.retention`；
   `0`/缺省 = 不启用该维度（既有全局语义继续生效）——**默认全关，零回归**。
2. **`audit_ttl` 只在默认卷生效**：审计是**默认卷单点权威**（审计日志 + 内存 ring 都在默认卷根），
   非默认卷配置审计龄 → 装配期 Warn + 忽略（不能假装每卷各有独立审计）。
3. **拒绝无意义的空转任务**：`gc_interval > 0` 但所有 TTL 为 0 ⇒ 配置校验直接拒绝；
   负值拒绝。理由：一个永不清理的周期任务只是白白唤醒。
4. **零回归判据是「不启动 goroutine」**：全 TTL 为 0 时装配路径不启动任何新 goroutine、
   不挂载 ticker，既有行为逐位不变。
5. **卷级 GC 与既有周期任务同构**（ticker + stop channel + WaitGroup，对齐 `mirror_interval` /
   `tier_policy` 模式），卷关闭即停止；不另造生命周期模型。

## 外部契约 / 踩坑
- 分享 TTL 是**兜底**语义：`share_ttl > 0` 时即使 `expire_at` 未到也按创建龄清理，
  与既有 share-cleanup 的 `expire_at` 语义**并存**（两套判据都生效，不是替换）。
- 只对「绑定本卷」的数据生效；跨卷/外部卷的保留语义不在本策略内。

## 落地指针
`pkg/server/config/config.go` 的 `VolumeRetentionConfig`（`VersionTTL` / `ShareTTL` / `AuditTTL` /
`GCInterval`，含 Validate）、`pkg/server/volume_retention.go`（清理执行器与卷级 ticker）。
