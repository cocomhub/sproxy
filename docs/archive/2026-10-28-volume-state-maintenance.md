<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一卷状态维护：卷维度 + 装饰器机制 — 归档

> **归档说明**：原文 `docs/designs/2026-10-28-volume-state-maintenance.md`（638 行）已于
> 2026-11-14 归档。现行事实源见 `docs/architecture.md`「卷状态维护（list / checksum / 索引的
> 统一缓存层）」（四条不变量 + 双上限 + 权威口径 + 写面纪律），门禁 R40/R41/R42/R43/R44/R45/R46
> 见 `docs/gates.md`（`pkg/volumestate` 为唯一 list 级缓存提供方）。原设计稿中「list 数据源收敛」
> 与「本地卷纳入缓存」两主题的后续实现已被设计 11（checksum 台账只读化）与设计 4（派生状态投影）
> 接续。本文件只保留**裁定与被否决方案 / 架构不变量 / 决策演进 / 仍有效判断 / 未决项 / 落地指针**。

## 背景
list 的目录视图与 checksum 派生值一度有**三套实现**（本地卷 `searchIndex` + `checksums.json`、
外部卷每次远端 `ListDir`、secretdata 自管索引）⇒ 三个真相源、三条失效路径（#874：旁路写漏一处即
「list 缺条目 + checksum 空」）。目标是把它们收敛为**唯一卷状态缓存**（`pkg/volumestate`）。

## 裁定：为什么**不**做「专门的缓冲卷」（被否决方案）
被否决：新增卷后端类型（`type: cache` / 缓冲卷）包在源卷前面提供 list/checksum 缓存。否决理由：
| 理由 | 后果 |
|---|---|
| **默认不生效** | 卷型须被配置声明才存在 ⇒ 存量外部卷仍每次远端 ListDir ⇒ 最痛点不解决 |
| **污染卷命名空间** | `cache` 卷会出现在 `/api/volumes`、ACL、配额、move/copy/rebalance、转存目标、mirror/backup…**每个卷面 API 都要加「排除 cache 卷」守卫** ⇒ 永久的持续修改负担 |
| **破坏 `Kind` 不变量** | 缓冲卷有本地字节 ⇒ `KindStorage` ⇒ 必须注册配额层，与「缓存不占用户配额」冲突 |
| **失效仍需纪律** | 卷型只解决读加速；写后失效仍要求每个写路径自觉通知 ⇒ #874 类缺陷原样存在 |

⇒ 采用**装饰器**（装配期自动包住每个卷，零配置）：成功先例是容量层（唯一入口
`capacity.WrapStorageVolume` + R29 门禁），本设计同形状：唯一入口 `volumestate.WrapVolume(...)`
+ **R44** 门禁（缓存唯一装配点）。「缓冲」仍存在，但是**受管落盘区**
（`<storage_root>/meta/state/<卷>/…`），由单一组件负责上限/GC/保密卷不落盘——**不是卷模型实体**。

## 架构不变量（长期稳定，四条）
- **P1 卷维度**：所有卷状态以「卷」为第一维，键恒为 `(volume, owner, rel)`；跨卷聚合是**对卷集合
  的派生遍历**，不是独立全局索引。⇒ 卷删除/卸载即整分区回收，无需全局失效广播。
- **P2 横切关注点 = 装配期装饰器**：与容量记账同类；业务代码只能拿到装饰后视图，**禁止**直接
  操作缓存。
- **P3 失效由「变更事件」驱动，不靠调用方登记**：写路径不需要知道缓存存在；变更事件有两个
  **原语级**产生点（覆盖全集）——`sync.FS` 写方法（外部/封装卷）+ `storage.Root` 写方法（本地卷）。
- **P4 卷自管 = 只报告事实，不实现缓存**：卷可实现可选能力（`ListDir` 填 checksum、
  `StateGeneration` 报告代际），但**不得**自建 list 缓存（**R40** 门禁）。

## 关键决策演进（保留「当时的判断 → 后来的推翻」，避免重走）
1. **本地卷 TTL**：原判断「必须有非零 TTL，否则进程外改动永不收敛」；**2026-10-29 被用户推翻**——
   默认改为 **0 = 永不过期**，进程外改动只能显式刷新感知（不鼓励用短 TTL 猜收敛窗口）。落地：
   `volume_state.disable: true` 才关闭（AGENTS 7c：默认开启需反转语义为 `disable_*`）。
2. **本地卷 `StateGeneration`（目录 mtime + 子项数）**：**不实现**——每次缓存命中都要额外一次
   `ReadDir` 取子项数（与「缓存避免列举」相反），且 POSIX 下改子目录内容不更新祖先 mtime ⇒ 祖先
   探测无效。显式 `POST /api/volumes/refresh` 已是明确契约。
3. **`checksums.json` 台账只读化**：**暂缓**（见下 §遗留）。
4. **`searchIndex` → TreeView 投影**：**未做**（见下 §遗留）。

## 失效机制（P3）与删除顺序
写路径无需知道缓存存在；唯一入口在 `WriteFile/Delete/Rename/MakeDir/WriteIfAbsent/Move/Copy/Link`
成功后**自动失效**（含祖先目录）。删除路径的顺序同 `docs/archive/2026-10-27-remote-derived-state-cache.md`
的删除三段式。

## 已实施要点（趋于稳定，保留决策性内容，删去实现步骤）
- **V2/V3/V4 各阶段**：Root 变异事件 → 本地卷索引自动维护；本地卷内容写（`Root.MarkWritten`）
  进索引；旁路写只补 checksum；跨卷搬移只迁移 checksum。
- **本地卷纳入缓存（§16）**：`wrapVolumeState` 不再跳过本地卷；`localVolumeMutationHandler` 把 Root
  变异**同时**投递给索引（`files.ApplyRootMutation`）与缓存（`volumestate.Cache.Invalidate`）；
  同卷复制快路径由 `CacheFS` 转发 `CopyFile` 并在成功后失效目标。
- **启动顺序踩坑（重要）**：**观察者必须在租户创建之前装好**——`RegisterRoutes` 的
  `initStorageLayout` **先建 anonymous 租户**，而 `files.Service` 懒装配；观察者若等到那时才装到
  卷根，已缓存的租户子根**不会继承**（`OpenTenant` 只在创建时继承）⇒ 该租户所有写都不发事件 ⇒
  缓存永不失效。修法：**两阶段接线**（装配早期装转发器 `installLocalVolumeObservers`，files.Service
  就绪后再把真正处理函数存入 `Handlers.mutationHandler`）。
- **内存上界（2026-10-30）**：双上限——目录快照数（`DefaultMaxEntries` 4096）与**总条目预算**
  （`DefaultMaxTotalEntries` 200_000，所有目录 entries 之和）；超预算按 LRU 淘汰；**单目录快照超总预算
  ⇒ 不缓存**（回源）。
- **校验巡检台账读取**：用可选扩展 `checksum.Iterator` 分批遍历（峰值 O(键列表 + 单批)），
  判空用 `LenCounter`（O(1)）；store 未实现则回落 `GetAll`（零回归）。

## 遗留（原文 §15，跨设计接续）
- **§15.1 台账只读化**：现状台账是 per-tenant 单 JSON（整体重写、可丢可损），对象 sidecar 已带
  `TotalSHA256`。**已修正确性缺陷**：台账键改卷维度 `<vol>\x00<rel>`（`VolumeKeyedStore`），
  旧无卷键只读兜底（升级不自动迁移）。**待做**：步骤 3「`Set/Delete/Rename` 不再写台账、读路径全量
  走 sidecar」——**前置口径**是「跨卷同名键空间」，且需先定读路径。后续由
  `docs/archive/2026-11-11-checksum-ledger-read-path.md` 接续（口径 = 视图条目自报优先 + 台账兜底）。
  **遗留裁决**（§15.1.3，逐项给定结论）：版本桶 checksum 键**不迁移**；旧无卷键**不自动迁移**。
- **§15.2 `searchIndex` → TreeView 投影**：把 `searchIndex` 拆成「结构视图（可投影）+ 搜索索引」
  需要先定结构视图来源、集群同步契约、内容/向量索引失效边界与 R41 落地时机。**已落地部分**：
  `files.List` 经**卷视图**逐卷 `ListDir` 聚合（`listViaVolumeViews`），能力不可用（`disable: true`）
  ⇒ 回落；聚合语义与索引路径逐条等价；**唯一列举入口** `VolumeViewLister.ListDirView`（`ExternalList`
  / `ExternalVolumeSource` 已删除）；步骤 3 删 `searchIndex.list`（`.index.list(` 硬禁，R41）。

## 落地指针
`pkg/volumestate`（唯一 list 级缓存 + 装饰器；`doc.go`）、
`pkg/volume`（可选 `StateGeneration`）、`pkg/storage/root_mutation.go`（Root 事件）、
`pkg/files/mutation_index.go`（索引投影）、门禁 R40/R41/R42/R43/R44/R45/R46（`docs/gates.md`）。
