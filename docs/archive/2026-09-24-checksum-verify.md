<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 全仓 checksum 巡检（11.3-①）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-checksum-verify.md` 已于 2026-11-13 归档。
> 端点与配置以 `docs/api.md`（`POST /api/verify`）与 `docs/config.md`（§全仓 checksum 巡检）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
per-request 校验工具与 per-tenant checksum 台账已存在，但**无全卷一致性审计**。目标：重算全部文件
SHA-256 比对台账 → 坏文件隔离/报告 + 定时巡检 + 告警联动。

## 定案与取舍（仍有效）
1. **巡检只隔离、不删除**：不一致文件 rename 到 `<tenant meta>/quarantine/` 保留现场，**人工确认后
   再删**（遵守「删除操作先确认」红线）。隔离目录落租户 meta（非用户可见桶，不污染卷视图）。
2. **单租户失败不中止全卷**：台账损坏/缺失 ⇒ 记 `skipped` 跳过；**读失败与 checksum 不符分开记**
   （不混淆「坏数据」与「读不到」）。
3. **不做锁**：巡检期并发上传可能瞬时不一致 ⇒ 报告标注 `concurrent` 提示重跑（以「读时刻」为准）；
   定时巡检与前次重叠 ⇒ 跳过本 tick（单飞防重入，不堆叠）。
4. **默认不开周期任务**：`verify_interval = 0`（默认）关闭周期巡检，**手动端点恒可用**（零回归）。
5. **隔离 rename 失败不静默**：记入 `errors`，文件留原位但列入报告。

## 落地指针
`pkg/server/verify`（`New` / `VerifyOnce` / `Report` / `Mismatch`）、路由 `POST /api/verify`、
配置 `verify_interval`；不一致联动告警 `source=checksum_mismatch`（需 `alerts.enabled` + 规则）。
