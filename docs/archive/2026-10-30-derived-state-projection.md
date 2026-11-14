<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 派生状态投影：单一写入口与事件语义 — 归档

> **归档说明**：原文 `docs/designs/2026-10-30-derived-state-projection.md` 已于 2026-11-14 归档。
> Phase 1 / Phase 2 已交付，**Phase 3（全卷搜索，基于缓存的实现）亦已交付**（原文末尾已注明）。
> 现行事实源见 `docs/architecture.md`「卷状态维护」的四条不变量与 R45/R46/R47 门禁说明、
> `docs/gates.md`（R47 `index_single_write_gate_test.go`）。本文件只保留**不变量 / 事件语义决策 /
> 被否决的中间层 / 快照安全判据 / 仍有效判断 / 落地指针**。

## 背景与问题
派生状态（搜索索引 / 卷状态缓存 / 台账）由**两条路径**维护：写内核显式调用
（`pkg/files/write_ops.go` 等直接 `s.index.upsert/remove/rename`）与 `storage.Root` 事件
（`ApplyRootMutation`）。后果：① 一次上传/删除做两次全量 COW 深拷贝（O(N)），写延迟与 GC 翻倍；
② 两条路径语义不一致时索引残留/缺失，且只有一条被测试覆盖；③ 事件无 `IsDir`，目录 rename 在
事件层只能删「单条」目录条目 ⇒ **子树条目残留**。

## 目标不变量（验收口径）
- **I1 单一写入口**：`user/` 桶的派生状态只由 Root 事件驱动；写内核**不再**直接调用索引写方法。
- **I2 事件语义自洽**：任何事件携带足够信息（`IsDir`）让消费方可做出与磁盘一致的投影；消费方
  不得依赖「另一个显式调用」补齐。
- **I3 快照安全**：索引快照经 `Root.OpenFileEncrypted` **加密写**（落在 `meta/index/<owner>.json`，
  卷内）；卷状态缓存因其落在**节点本地明文目录**才需要 `volume_state.persist_private`——两者口径
  不同，索引无需额外开关。
- **I4 可观测**：索引/缓存的规模、命中、重建、落盘跳过均有指标。
- **I5 门禁**：写内核出现 `s.index.<write>` 即红（R47）。

## 决策与取舍（仍有效）
1. **`Mutation.IsDir` 事件语义**：`Root.Remove`/`Rename` 在底层操作**前** `Stat` 一次填充（低频，
   成本可忽略）；`Root.RemoveAll` 恒 `IsDir=true`；`Mkdir` 恒 true；`Write`/`Chtimes`/`Link` 为
   false。消费方：`MutationRemoveAll || IsDir` ⇒ `removePrefix`；rename 目标 `IsDir` ⇒ `upsertDir`。
   `Stat` 失败（目标已被并发删除）⇒ 保守 `false`（与现状一致，不引入新错误）。
2. **投影器不新造包**（被否决方案）：初稿设想新建 `pkg/derived`，实施改为**原地扩展**
   `ApplyRootMutation`（`mutation_index.go`）——避免为「只有一个消费者」引入中间层。
3. **快照安全 D3 的正确判据（重要，避免错误结论）**：原假设「索引快照会明文落节点磁盘」**不成立**
   （经 `Root.OpenFileEncrypted` 写卷内）。**不要**复用 `volumePrivate`（对 `DirectLink=false` 的
   本地卷返回 true）——那会让索引退化为每次重启全量扫描。
4. **测试基建必须与生产同构**：`dirsEnv` 改为「观察者装**卷根** + 租户经 `storage.OpenTenant` 子根
   创建」（事件 `Rel` 自动带 `<owner>/` 前缀）。此前用 `OpenRoot(<root>/<owner>)`（无前缀机制）⇒
   事件驱动看似不生效，**掩盖了真实覆盖情况**。

## Phase 3 全卷搜索（已定案并实施，保留边界）
用户裁定（2026-10-30）：**基于缓存能力提供全卷缓存实现搜索，支持对有权限访问的所有卷搜索并聚合**。
- **数据源**：本地卷走 `searchIndex`（内存 O(N)）；**外部卷**走 `VolumeViewLister.ListDirView`
  （唯一目录列举入口，视图链含 `volumestate.CacheFS`：TTL=0 永不过期 + 写穿 + `volume refresh`）。
- **聚合**：本地 + 外部递归结果合并 → 统一排序（目录在前 → name asc → volume asc）→ 可选分页；
  目录条目跨卷按 name 去重，文件带所属卷名。
- **上限（必须）**：深度 ≤ 16、单请求总条目 ≤ 50000（跨全部外部卷共享）、超时 10s（ctx 派生）；
  超限截断并 Warn（**不做静默无限遍历**）。
- **失败隔离**：单卷/单目录失败 ⇒ Warn + 跳过，本地结果与其他卷不受影响（搜索不整体失败）。
- **`tag` 过滤**：`tag` 是本地卷元数据 ⇒ `tag` 非空时**不遍历外部卷**（外部卷无标签语义）。
- **不做**（明确）：外部卷独立索引（方案 B，缓存层已提供同等加速，额外索引是第二份派生状态）；
  实时递归（方案 C，无缓存保障）。

## 风险与回滚
- Phase 2 后写内核与索引彻底解耦：**新增写面若不经 `storage.Root`，索引不会更新** ⇒ 由 R47 +
  契约测试 + E2E 兜底；新增本地写路径必须经 Root（「按卷操作」纪律的一部分）。
- `RecordBypassWrite` 的 upsert 仍是**外部卷**唯一入口（无 Root 事件）；本地旁路写应改用
  `RecordBypassChecksum`（只补台账，索引由事件维护）。

## 落地指针
`pkg/storage/root_mutation.go`（发行事件）、`pkg/files/mutation_index.go`（`ApplyRootMutation` 唯一投影）、
`pkg/server/volume_mutation_wiring.go`（装配接线）、`pkg/files/search_external.go`（Phase 3 全卷搜索）、
门禁 R45（Root 变异发事件）/R46（旧派生状态棘轮）/R47（单一写入口）。
