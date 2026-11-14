<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# secretdata 索引快照：热/冷分层与元数据缓存组合策略 — 归档

> **归档说明**：原文 `docs/designs/2026-11-12-secretdata-index-lazy-meta-and-cache-layers.md` 已于
> 2026-11-14 归档。**P1（快照去 Meta + `origSHA256` 提升为 L0）/ P2（L1b 内存 LRU）/ P3（L1a 磁盘
> 配额 + miss 集批量加载）/ P5（字段级别路由）已交付，P4（预热）亦已落地（`disable_meta_prewarm`
> 默认 false = 开启）**。现行事实源见 `docs/secret-volume.md` §3.10（分层与配置）与 §3.2 配置表。
> 本文件只保留**问题定论与实证数据 / 根因（已有设计被绕过）/ 四层模型 / 命中率组合策略与实测修正 /
> 被否决的路线 / 落地指针**。

## 问题定论（实证，非推断）
加密卷（secretdata）启动期内存峰值可达 **GB 级**（WSL 内被 OOM killer 杀死）。实测：单文件
meta JSON **87 MB**，反序列化后 Go 存活内存 **78 MB**（23 chunks / **430,652 blocklets**）；
本地 metacache 目录实测 **9,489 文件 / 5.29 GB**，最大单文件 **86.98 MB**（中位数仅 1,217 B）
⇒ 只要**少数大文件**的 meta 进内存就几 GB 常驻。
**三轮假设推翻记录**（供后人不再重走）：① 「主体是 `Dirs`/`DirSegs`/`DirIDs`」✗（30 万目录合成
夹具 gzip 仅 7.3 MB）；② 「`ChunkInfo.FileName` 重复」✗（占 0.001%）；③ **主体是
`Chunks[].Blocklets`，且本不该常驻** ✓。

## 根因：冷热耦合，且**已有设计被绕过**
`metaEntry.meta *shardseal.Meta` 让每个文件常驻完整 Meta；而热路径（`ListDir` 不访问；`Stat` 只读
`Original.SHA256`）根本不用它。**按需加载机制早已存在**（`ensureEntryMeta` / `readFileMetaFast`）
——设计意图本是「meta 可为 nil，用时再读」，**快照把 `Meta` 全量填满，把这个设计废掉了**。
⇒ 结论：不是「缺设计」，而是「**已有设计被绕过**」；修复方向是**让快照回到设计意图**，不新造架构。

## 四层数据管理（核心改动只有一条）
- **L0 热索引**（本地快照，必常驻，零远端）：`rel→{size, mtime, origSHA256, dirSeg, metaName,
  dataDir, baseVersion, phys}`；覆盖 list/stat/search/目录浏览；~150 B/文件（850 文件 ≈ 130 KB，
  对比现状快照 241.8 MB）。
- **L1a 元数据磁盘缓存**（`metacache`，持久 + 配额 + LRU）：跨重启有效。
- **L1b 元数据内存缓存**（LRU，**字节计量 + 硬上限**）：仅缓存「最近真正被读取」的 meta，绝不随卷增长。
- **L2 远端 meta blob**（权威）：只在 L1a/L1b 未命中时读。
**核心改动**：快照**不再序列化 `Meta`**，加载后 `e.meta = nil` 交给既有 `ensureEntryMeta`；把
`origSHA256`（← `meta.Original.SHA256`）提升为 **L0 扁平字段** ⇒ `Stat` 不需要 meta。

## 命中率组合策略（本方案重点）与实测修正
- **按「所需字段级别」路由（收益最大）**：批量操作先声明需要什么，能在 L0 满足的绝不碰 meta
  （列目录/stat/搜索/checksum 巡检只读 L0；下载/纠删恢复/深度校验才需 blocklets）。
  实施记录（2026-11-13）：新增可选能力 `meta.ChecksumProvider` + `AssertChecksumProvider`，
  `secretdata` 用热字段实现（**完全不加载冷结构**）；已切换 `files_service.ExternalStat`（外部卷 stat
  热路径）/`ExternalCopy`/`cloudtasks.recordArchiveBypassState`；**确实需要**冷结构的
  （`cloud/transfer.go` 分块级校验、extra 读取）保持 `FileMeta`。
- **批量两阶段**：全量查 L1b→L1a → 只对 **miss 集**走 L2。
- **回填 + 去重（single-flight）+ 限速**；**预热（可选）**只写 L1a（不增常驻内存）。
- **缓存淘汰**：L1b 按字节 LRU 硬上限；L1a 磁盘配额 + LRU。**正确性红线**：L1a/L1b **永不作为唯一
  真相**，未命中/校验失败 ⇒ 回落 L2。
- **⚠️ 实测修正（2026-11-13）**：原文假设「回填后同类批量操作第二次全命中」**只在小热集成立**——
  夹具单条 ≈ 18.2 MB、默认预算 256 MiB ⇒ 容量 14 条，批量 20 条第二轮命中 70%；而**真实**大文件
  meta 常驻 **78 MB** ⇒ 256 MiB 只容纳 3 条 ⇒ 全仓第二轮命中率 ≈ **0.35%**（几乎全 miss）。
  ⇒ **结论修正**：L1b 只能保证「热集 ≤ 预算」时的复用；**批量「大概率命中」只能靠「按字段级别路由」
  ——即根本不加载 meta**，这也把 P5 的优先级抬到 L1b 扩容之上。
- **并发上限实测（P3 决策依据）**：真实生产 meta（430,652 blocklets / 86.98 MB）解密+反序列化的
  吞吐在**并发 4 之后严重饱和**（8→16 只增 7% 却多花 1.5 GB），**内存 ≈ 并发 × 单条常驻**
  （单条约 250–350 MB）⇒ **内存才是瓶颈**；上限建议 `min(4, 内存预算/单条常驻)`，**必须硬性封顶**
  （不能只靠配置）。未测：远端下载耗时（真实链路总耗时）。

## 被否决的路线（为什么不做）
| 路线 | 为什么不做 |
|---|---|
| **按目录聚合清单缓存** | ① 引入聚合一致性（目录级锁 + 重试闭环）；② 热字段提升到 L0 后已无必要；③ L0 是扁平索引，比聚合更简单 |
| **WAL + 定期压缩** | 解决的是**写路径写放大**，而本问题瓶颈是**读路径内存**；且引入重放窗口/压实时机/崩溃恢复三类复杂度，还要与 `pkg/recovery`、`pkg/audit` 划界（违反「同一语义只留一份实现」） |
| **`ls -l` 取 checksum 做新鲜度** | 只影响「L0 的 checksum 是否新鲜」，**不是本方案成立的前提**；可后续单独评估 |

## 与相关设计的边界
本仓有两份**方向互补、易混淆**的设计（归档后均见 `docs/archive/`）：
- **本文**：`secretdata` **单个文件的冷结构 `Meta`** 如何不进热内存（启动内存/读取成本）——已交付。
- `2026-11-11-checksum-ledger-read-path.md` §10：**目录级 checksum 索引**（`dirindex`，方案 D，让本地
  checksum 台账退场）——建议先做 D1。两者对象不同（文件冷结构 vs 目录 checksum 聚合），**互不依赖**；
  `secretdata` 的 `KindDirMeta` 是**逻辑树**（改名/移动权威记录），新方案须**另起名 `dirindex`**。

## 落地指针
`pkg/volume/secretdata/index_snapshot.go`（快照去 Meta + `origSHA256`）、`meta_cache.go`（L1a/L1b +
配额 + 并发闸）、`meta_prewarm_*`（预热）、`pkg/sync` 的 `meta.ChecksumProvider`（字段级别路由）、
`pkg/server/config`（`meta_cache.*`）、`docs/secret-volume.md` §3.10/§3.2。
