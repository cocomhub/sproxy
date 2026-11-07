# index / cache / search 收敛度审计与修复（2026-10-29）

> 结论先行：**list / cache / checksum 已收敛**（唯一数据源 + 事件写穿 + 显式刷新 + 兜底）；
> **search 曾是独立缓存**，与 refresh 脱节、失效依赖删除快照、无内存上限与指标。
> 本系列（#905 / #906 + 本片）把缺口补齐；剩余一项（写内核双写）需先统一测试基建，已记档。

## 一、审计结论（修复前）

| 维度 | list（卷状态视图） | search（searchIndex） | checksum 台账 |
|---|---|---|---|
| 数据源 | 卷 `ListDir`（CacheFS）→ 实时扫描兜底 | 内存索引（构建 + 写路径增量） | 内存 map + JSON |
| 持久化 | `volumestate.Store` | `meta/index/<owner>.json`（5m 周期） | `checksums.json` |
| 失效 | 写穿（装饰器 + Root 事件）+ 显式 refresh | 写路径增量 + Root 事件 + 旁路 `InvalidateIndex` | 写内核 + 删除按卷 |
| 崩溃恢复 | 载入快照（TTL=0 ⇒ 直接用） | 载入快照（**无校验**） | 载入 JSON + meta 重建 |
| 兜底 | ✅ 实时扫描 | ❌ 唯一来源 | ✅ `RebuildChecksums` |
| 容量约束 | ✅ LRU 4096 + Store 上限 | ❌ 无上限 | 全量 map |
| 指标 | ❌ | ❌ | ❌ |

## 二、已修（#905）

1. **失效代际化**：快照带 `Gen`（per-owner）；`InvalidateIndex` 递增 ⇒ 删除快照失败/被复活也不会载入。
2. **全局失效**：`InvalidateAllIndexes` 清内存 + 删除全部快照（StateStore `List("index/")`；本地 JSON
   枚举 tenant 的 `meta/index/*.json`，含本进程未知 owner）⇒ 跨进程有效。
3. **refresh 联动**：`POST /api/volumes/refresh`（全部/单卷）同步失效搜索索引（此前只清 list 缓存）。
4. **崩溃窗口**：写路径置 `pendingSave`；`indexSaveLoop` 加 1s debounce 只落变更 owner（原 5m 周期），
   取消「启动先 save」（那会把陈旧快照固化为当前态）。

## 三、已修（#906）

5. **LRU**：`defaultMaxIndexOwners = 128`；淘汰只丢内存索引（快照/代际保留 ⇒ 按需重建）。
6. **锁外 I/O**：`removePrefix` 的标签清理移出 `ix.mu`（rmdir 大目录不再阻塞搜索）。
7. **指标**：`sproxy_volume_cache_{hits,misses}_total`、`sproxy_volume_cache_entries`、
   `sproxy_search_index_{owners,entries}`、`sproxy_search_index_rebuilds_total`。

## 四、已修（本片）

8. **搜索可选分页**：`offset`/`limit`（不传 = 全部，兼容既有契约；`total` 恒为全部命中数）。
9. **等价性测试**：`TestSearch_IndexMatchesDiskWalk` 以磁盘遍历为判据独立核对索引结果。
10. **文档漂移修正**：`volume_state_wiring.go` 的「本地卷 list 仍走 searchIndex」注释已过时。

## 五、未做 / 仍开（如实记录）

- ~~**写内核显式索引增量 vs Root 事件的双写**~~ → **已修（2026-10-30，#908 + Phase 2）**：
  目录 rename 事件补 `IsDir` 与目标子树重建；测试基建改 `OpenTenant`（观察者装卷根）后删除
  写内核全部 5 处 `s.index.<write>`；R47 门禁 + R46 棘轮锁定（详见
  `docs/designs/2026-10-30-derived-state-projection.md`）。
- **外部卷文件不可搜索**：与「默认聚合只含本地卷」的历史语义一致；显式 `?volume=` 的 list 已有，
  搜索侧若要支持需递归列举外部卷（成本高），留待产品口径。
- **`checksumSnapshotsByVolume` 每次 Search 全量拷贝台账**（O(N)）：大库有毫秒级成本，
  未做惰性化（收益有限，且需改 `searchLocked` 的数据形状）。
- **目录子树 rename 的显式索引搬移保留**：`storage.MutationRename` 只有单条语义（Rel/Rel2），
  不表达子树 ⇒ 该处显式调用是**必要实现**，不属于冗余双写。

## 六、仍有效判断

- **flake/缺陷先取证到根因**：本次三类问题（refresh 脱节、失效依赖删除、无上限）都有代码级证据，
  修复均可用「变异命中」验证。
- **失效不要依赖删除成功**：删除是 best-effort（权限/占用/崩溃窗口），正确性应由**代际**保证。
- **缓存/索引必须有上限 + 指标**：无上限的派生状态在多租户下必然成为内存泄漏；无指标则
  「缓存是否生效/索引是否陈旧」只能靠猜。
