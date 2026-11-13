<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 审计日志轮转（roadmap 11.5-⑥）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-audit-rotation.md` 已于 2026-11-13 归档。
> 配置字段与默认值见 `docs/config.md`（现行事实源），本文件只保留**设计意图 / 取舍 /
> 外部契约 / 仍有效的判断**。

## 背景
审计 store 双写「内存全量 + append-only JSON lines」，`<persist_dir>/audit.log` **只增不删** ——
长期运行磁盘无界增长；且启动会全量载入日志。目标：按大小轮转 + 保留 N 份归档，且
**内存/查询语义完全不动**。

## 定案与取舍（仍有效）
1. **默认关闭（`max_size=0`）**：未配置时行为与现状逐字节一致（零回归）——审计链路上的任何变化都必须
   是显式开启的。
2. **轮转全程持同一把锁**：`Append` 本就持锁 ⇒ 轮转天然串行，不存在并发 Rename。
   这是刻意的设计（而非"顺手"），因为 Windows 上并发 rename 会失败 —— 不要在锁外做轮转。
3. **轮转失败一律尽力而为**：Close/Rename/Open 任一失败 ⇒ 记日志 + **不轮转、继续 append 原文件**，
   归档移位删除失败 ⇒ Warn 继续、残留由下次轮转清理。审计写入**绝不阻断业务**（与既有
   「写盘失败记日志跳过」语义一致）。文件 stat 失败 ⇒ 跳过轮转检查直接写（退化为现状）。
4. **恢复语义固化（关键判断）**：重启**只载入当前段**（`audit.log`），内存 = 热历史（有界）；
   归档是**离线合规冷数据，不载入内存** —— 否则「轮转省磁盘」会换来「重启内存暴涨」，把问题换个位置。
5. **边界语义**：`size == max_size` **不轮转**、`> max_size` 才轮转；`max_archives = 0` = 轮转即删不留档。
   归档序号方向：`.1` = **最新**归档，超 `max_archives` 删最旧。

## 外部契约 / 踩坑
- 归档文件名序列（`audit.log.1 … audit.log.N`，`.1` 最新）与「超限删最旧」是**运维/合规可见**的契约：
  外部采集/归档脚本按命名与序号取件，改命名或改排序方向会影响它们。
- **与外部 logrotate 会双轮转**：启用内置轮转时应关闭外部 logrotate（文档已明示冲突）。
- 保持「内存/查询语义不动」是验收红线：轮转只影响**落盘布局**，不是一次审计模型改造。

## 落地指针
`pkg/audit`（`MaxSize` / `MaxArchives`；`pkg/audit/journal.go` 的 `rotateIfTooBigLocked` / `rotateLocked`
—— 设计稿里的方法名 `maybeRotateLocked` 与最终实现不同）、
`pkg/server/config`（`audit.max_size` / `audit.max_archives`，已收录于 `docs/config.md`）。
