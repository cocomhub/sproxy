<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 告警规则热加载（11.1-②）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-alert-hot-reload.md` 已于 2026-11-13 归档。
> SIGHUP 热重载范围以 `docs/config.md`（§SIGHUP 热重载）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
SIGHUP 重载原先仅 `log_level`/`log_format` 生效，改告警阈值/渠道必须重启进程。目标：补
`notify.alerts.rules` 热加载——规则集**原子替换**，渠道实例不重建。

## 定案与取舍（仍有效）
1. **原子替换 + 保留 firing 状态**：锁下 `slices.Clone` 替换规则切片（读方持锁快照，无撕裂）；
   **state 不清空**——规则变更不误发恢复通知、也不重复告警。这是本设计的核心正确性要求。
2. **生效时机是语义约束**：阈值类 source（disk_watermark / quota_watermark）下个轮询 tick 生效
   （≤ `poll_interval` 延迟）；事件类 source（volume_degraded / sync_failed / login_locked /
   nat_failure）下个事件即生效。
3. **`alerts.enabled` 翻转仍需重启**（装配期决策，渠道 `AdoptNotifyChannels` 不可逆）——SIGHUP
   只换规则、不重建装配；这是**有意限制**，不是遗漏。
4. **配置解析失败保持旧配置、不部分应用**（沿用既有 handleSighup 语义）。

## 落地指针
`pkg/server/alerts/alerts.go`（`Engine.ReloadRules`）、`cmd/sproxy/root.go`（handleSighup 接线）。
