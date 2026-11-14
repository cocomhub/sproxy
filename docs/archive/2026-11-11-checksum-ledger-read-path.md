<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# checksum 台账只读化：读路径口径裁定 — 归档

> **归档说明**：原文 `docs/designs/2026-11-11-checksum-ledger-read-path.md` 已于 2026-11-14 归档。
> 该文是「读路径口径」的**决策文档**，对应 `docs/archive/2026-10-28-volume-state-maintenance.md` §15.1
> 台账只读化遗留项。现口径已记入 `docs/gates.md`（D4：视图条目自报 checksum 优先 + 台账兜底）；
> 本文件只保留**口径裁定 / S1–S2 落地事实 / S3 阻塞点 / 候选方案 D / 仍有效边界与判断**。
> 分期清单与实现细节已移除。

## 背景与现状事实（代码级，仍有效）
1. **卷自报 checksum 机制已存在**：`sync.Entry` 早有 `Checksum`/`ChecksumType`/`Checksums`；
   secretdata 在 `ListDir` 填整文件 SHA-256。
2. **搜索路径已用卷自报**，但**列表路径丢弃卷自报**（只查台账）⇒ 同一份数据 `/api/files` 与搜索
   口径不一致，secretdata/S3 的自报 checksum 在 list 上被浪费。**这是正确性/一致性缺陷**。
3. **本地卷 FS 链没有 meta 读取能力**：`meta.Provider` 的实现者只有装饰器与 `volumestate.CacheFS`；
   本地卷核心 `rootfs` 不实现。本地卷 meta sidecar 由 `pkg/files` 经 `*storage.Root` 读写。

## 口径裁定（采纳方案 A 的落地形态）
**读口径 = 「视图条目自报 checksum」为第一优先，「台账」为过渡期兜底；本地卷的自报由列举缓存层
（`volumestate.CacheFS`）补齐。**
- **为什么不是「让 `rootfs` 读 sidecar」**（被否决方案）：meta 的物理布局（`.meta` sidecar 路径、
  加密卷差异）是 `pkg/files` 的领域知识，下沉到卷层即**架构倒置**。
- **方案 C（list 逐文件读 sidecar）弃用**：大目录 N 次 I/O（台账正是为避开它而存在）。
- 归档时**重要修正**：S2 实施把落点从「装配层注入 `metaReader`」**收敛为 `CacheFS` 内部**——
  cache-miss 的 `fetch` 内用 `inner.(meta.Provider)` 读 sidecar（`trusted.Wrap` 已实现 `FileMeta`，
  R21 要求装饰器全量转发 ⇒ 本地卷 ViewUser 链本就有该能力），填充**在 fetch 内** ⇒ 随目录快照缓存
  并落盘。**仅本地卷**（远端卷每文件一次网络往返 = 性能回归）。

## S1–S2 落地（已交付）
- **S1 口径对齐**：`fileInfosFromViewEntries` 优先 `e.Checksum`（未自报时回落台账），与 search 同口径。
- **S2 本地卷自报**：`volumestate.CacheFS` 在 cache-miss 回源后补齐本地卷条目 checksum
  （已有自报值/目录条目跳过；sidecar 缺失留空），**无需装配层改动、不新增 R28**。
- **验收口径修正（重要）**：原 S2 验收「清空台账后 list 仍正确」**不具判别力**——台账为空时现有
  自愈路径（`RebuildChecksums` 从 sidecar 批量回填）已能让 list 正确；S2 的可观测收益是
  **「列表 checksum 直接来自 sidecar，无需台账/自愈」**（性能与单一事实源）。

## S3 侦察结论：暂不具备安全增量条件（未做）
停写台账会同时影响两条**尚无替代来源**的读路径，须先裁定语义：
| 读路径 | 现状来源 | 停写后果 | 需裁定 |
|---|---|---|---|
| 本地 **search** 结果 checksum | 台账（`search_index.go`） | 搜索结果 checksum 变空 | 让 search 也走「视图/侧车」还是接受空值（客户端 `stat` 兜底） |
| **远端卷**（pikpak/webdav/sftp/ftp）**列表** checksum | 台账（这些 FS 的 `ListDir` **不自报** sha256） | 远端列表 checksum 变空 | 远端是否接受「空 + 按需 `stat`」（会改变现有 UX） |
其余路径已有替代或可接受（本地 list → S2；本地 stat → 台账 miss 时现算；s3/baidupcs 自报）。
⇒ 结论：S3 不是「窄化写路径」的小改，而是「本地 search 与 list 统一数据源」+「远端卷列表 checksum
语义裁定」两项前置；在裁定前**保持写台账**。S4（删门禁）随之推迟。

## 候选方案 D（用户提议 2026-11-12，方向正确，未决）
**为本地/可信卷增加「目录 checksum 索引」**：每目录一个聚合 meta（建议另起名 `dirindex`，与
secretdata 的 `KindDirMeta`（逻辑树）区分），内容 `{version, entries:{name:{sha256,size,mtime,type}}}`；
写路径在 `trusted.Wrap` 的写/删/改名后**同事务**更新父目录索引；读路径 list 一次读一个文件即得
全目录 checksum。
- **关键设计点**：一致性（索引更新失败 ⇒ 复用 `pkg/server/issues` 重试闭环，**不得静默**）；
  仲裁/版本（带 `version` 单调递增 + 目录 `mtime` 兜底，不符时按条目校验并重建）；并发（目录级锁，
  跨进程需 `pkg/server/filelock`）；迁移/自愈（存量首次列举重建）；规模（分片 + 纳入条目预算）；
  能力形态（可选能力接口 `DirIndexReader/Writer` 放 `pkg/sync`，装饰器转发，消费方 `Assert*`）。
- **取舍**：能把「本地侧台账」彻底退场（解锁 S3/S4），代价是**写放大 + 目录级并发/一致性**复杂度。
  建议 D1 格式+写路径 → D2 读路径 → D3 迁移/自愈 → D4 台账只读化，**先做 D1 并保持读路径不变**。
- **S2（逐文件 sidecar + 快照缓存）是方案 D 的回退路径，不废弃**。
- **待裁定**：是否按 D1–D4 推进（新增写放大与目录锁）？或保持现状（S2 + 台账只读兜底），仅做只读收敛？

## 仍有效的边界
- **远端卷**（S3/baidupcs）：每文件读 checksum = N 次往返 ⇒ 维持「不主动补」策略；消费方按需 stat 兜底。
- **加密卷**：`Entry.Checksum` 必须是**明文** sha256（secretdata 已是明文 meta）；本地加密卷经
  `CacheFS` 时不得把密文哈希填入（S2 的 reader 必须走 `pkg/files` 明文 meta，而非卷内原始字节）。
- **缓存一致性**：进程外改动遵循既有「显式 refresh / 默认永不过期」契约（§15.3 决策 b）。
- **不做**：不为「本地卷 StateGeneration」等低收益项立项；不改 `searchIndex`（独立议题）。
- **注**：新增注入式读取属**构造注入**（结构体私有字段 / opts），符合「测试注入纪律」（禁包级 seam 替换）。

## 与门禁的关系
R46（`legacy_state_usage`）的下降应由 S1→S3 **真实消除**驱动，**不得**通过改口径取巧。

## 落地指针
`pkg/volumestate/wrap.go`（CacheFS 自报补齐）、`pkg/files/read_ops.go`（list 取 `e.Checksum`）、
`pkg/files/search_external.go`、`docs/gates.md`（D4 口径 + R46 进展）。
