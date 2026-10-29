# 派生状态投影：单一写入口与事件语义（设计）

> 状态：**Phase 1 + Phase 2 已实施**（2026-10-30）；Phase 3（外部卷搜索）待产品口径。
> 范围：搜索索引（`pkg/files`）、卷状态缓存
> （`pkg/volumestate`）、checksum 台账（`pkg/checksum`）的**写入口收敛**。
> 前置审计：`docs/archive/index-cache-search-convergence.md`（#905/#906/#907）。

## 1. 背景与问题

派生状态（搜索索引 / 卷状态缓存 / 台账）目前由**两条路径**维护：

| 路径 | 触发 | 覆盖面 |
|---|---|---|
| 写内核显式调用 | `pkg/files/write_ops.go` / `service.go` / `tags.go` 直接 `s.index.upsert/remove/rename` | 只覆盖「记得登记」的写面 |
| `storage.Root` 事件 → `ApplyRootMutation` | 所有经 Root 的本地写（含旁路：转存/归档/镜像/备份） | 结构性事件自动；内容写靠 `MarkWritten` |

后果（正确性 / 可维护性）：

1. **双写**：一次上传/删除会做两次全量 COW 深拷贝（O(N)），写延迟与 GC 压力翻倍；
2. **漂移风险**：两条路径语义不一致时（例如目录 rename）索引残留/缺失，且**只有一条**会被测试覆盖；
3. **事件语义不足**：`Mutation` 无 `IsDir`，目录 rename 在事件层只能删「单条」目录条目，**子树
   条目残留**（搜到已不存在的文件）；目录删除依赖 `MutationRemoveAll`，而 `Root.Remove` 对空目录
   发的是 `MutationRemove`（残留）；
4. **安全缺省不完整**：索引快照落盘未对齐「保密卷不落盘」口径（卷状态缓存有
   `volume_state.persist_private`，索引没有）。

## 2. 目标不变量（验收口径）

- **I1 单一写入口**：`user/` 桶的派生状态只由 Root 事件驱动；写内核**不再**直接调用索引写方法。
- **I2 事件语义自洽**：任何事件（含 rename/remove）携带足够信息（`IsDir`）让消费方做出与磁盘一致的
  投影；消费方不得依赖「另一个显式调用」补齐。
- **I3 快照安全**：索引快照经 `Root` 的 `OpenFileEncrypted` **加密写**（加密卷上内容为密文）；
  卷状态缓存因其落在**节点本地明文目录**才需要 `volume_state.persist_private`——两者口径不同，
  索引无需额外开关（Phase 1 已核实，见 D3）。
- **I4 可观测**：索引/缓存的规模、命中、重建、**落盘跳过**均有指标（#906 已落大部分，本设计补
  `persist_skipped_total`）。
- **I5 门禁**：写内核出现 `s.index.<write>` 即红（R47）；投影器只允许一个装配点（R48 拟定）。

## 3. 设计

### D1 事件语义扩展（Phase 1，本片）

`storage.Mutation` 增 `IsDir bool`：

- `Root.Remove` / `Root.Rename` 在底层操作**前** `Stat` 一次（低频操作，成本可忽略）填充；
- `Root.RemoveAll` 恒 `IsDir=true`（递归删除语义；文件目标也按「可能含子树」处理）；
- `Root.Mkdir` 恒 `IsDir=true`；`Write`/`Chtimes`/`Link` 为 false（文件语义）。

消费方（`pkg/files/mutation_index.go`）：

- `mutationDropOnly`：`m.Op == MutationRemoveAll || m.IsDir` ⇒ `removePrefix`（否则 `remove`）；
- rename 目标：`IsDir` ⇒ `upsertDir`；**子树条目**由写内核的 `index.rename`（内存搬移）或下一次
  全量重建补齐——Phase 2 改为「投影器递归重建目标子树」，届时删除显式 `index.rename`。

### D2 单一写入口（Phase 2，**已实施**）

实施要点（与初稿的差异记录）：

- **投影器不新造包**：`ApplyRootMutation`（`mutation_index.go`）已是唯一投影入口，原地扩展
  （目录 rename → `applyDirRename`）而非新建 `pkg/derived`——避免为「只有一个消费者」引入中间层；
- 删除 `write_ops.go` 的**全部** `s.index.<write>`（upload/mkdir/rmdir/delete/rename 共 5 处）；
  `tags.go` 的 `setTags` 保留（标签不是 Root 事件，属显式写面）；
  `service.go` 的 `RecordBypassWrite` 保留（**外部卷**无本地 Root，事件不可达）；
- **测试基建统一形态**：`dirsEnv` 改为「观察者装**卷根** + 租户经 `storage.OpenTenant` 子根创建」
  （与生产同构：事件 `Rel` 自动带 `<owner>/` 前缀、`m.Root` 恒为卷根）。此前用
  `OpenRoot(<root>/<owner>)`（无前缀机制）⇒ 事件驱动看似不生效，掩盖了真实覆盖情况；
- **R47 门禁**：`write_ops.go` 出现 `s.index.<write>` 即红（含扫描面探针）；R46 棘轮同步降
  `write_ops.go` 4 → 0。

### D3 快照安全（Phase 1：已核实，**无需代码变更**）

原假设「索引快照会明文落节点磁盘」经核实**不成立**：

- `saveIndexSnapshot` 经 `storage.Root.OpenFileEncrypted` 写（`meta/index/<owner>.json` 在**卷内**）
  ⇒ 加密卷上快照内容为密文；
- `volumestate.Store` 的 `persist_private` 是另一回事：它落**节点本地明文目录**（非卷根），
  所以需要按卷关闭；索引快照不在此列。

⇒ 保持现状（零代码变更），避免引入「所有本地卷都被判私密 ⇒ 索引永不落盘」的错误判据
（`volumePrivate` 对 `DirectLink=false` 的本地卷返回 true，直接复用会让索引退化为每次重启全量扫描）。

### D4 全卷搜索（Phase 3，**已定案并实施**）

用户裁定（2026-10-30）：**基于缓存能力提供全卷缓存实现搜索，支持对有权限访问的所有卷搜索并聚合**。

- **数据源**：本地卷继续走 `searchIndex`（内存 O(N) 遍历）；**外部卷**走 `VolumeViewLister.ListDirView`
  ——装配层唯一目录列举入口，其视图链含 `volumestate.CacheFS`（TTL=0 永不过期 + 写穿 +
  `volume refresh`）⇒ 首次遍历付远端成本，之后命中内存缓存；
- **聚合**：本地结果 + 外部递归结果合并 → 统一排序（目录在前 → name asc → volume asc）→ 可选分页；
  目录条目跨卷/跨本地-外部按 name 去重（`Volume` 空），文件带所属卷名；
- **上限（必须）**：深度 ≤ 16、单请求总条目 ≤ 50000（跨全部外部卷共享）、超时 10s（ctx 派生，
  客户端断开亦取消）；超限截断并 Warn（不做静默无限遍历）；
- **失败隔离**：单卷/单目录列举失败 ⇒ Warn + 跳过，本地结果与其他卷不受影响（搜索不整体失败）；
- **权限**：只遍历 `volume.AllowedVolumes`（ACL 视图）；
- **标签**：`tag` 是本地卷元数据 ⇒ `tag` 非空时不遍历外部卷（外部卷无标签语义）；
- **不做**（明确）：外部卷独立索引（方案 B）——缓存层已提供同等加速，额外索引是第二份派生状态；
  实时递归（方案 C）——无缓存保障，已被本方案取代。

验收（变异均命中）：外部卷顶层/子目录命中并带 `Volume`（去掉分支 ⇒ 0 命中红）；
自嵌套目录受深度/条目上限约束（去掉上限 ⇒ 断言红）；外部卷失败隔离（本地结果保留）；
`tag` 过滤不触发外部遍历。

## 4. 分期与验收

| 阶段 | 内容 | 验收（变异必须命中） |
|---|---|---|
| **Phase 1**（本片） | `Mutation.IsDir`（Root 操作前 Stat 判定）+ 目录 rename 全投影（删源前缀 + 按磁盘重建目标子树）+ 删除写内核显式 `index.rename` + R45 门禁接受 `notifyMutationKind` | 目录 rename 后源前缀条目消失且目标子树路径正确（变异：改回单条 ⇒ 「应命中 z/b/keep.txt，got a/b/keep.txt」红）；`IsDir` 语义（Mkdir/RemoveAll true、文件 write/rename/remove false、目录 rename/remove true） |
| **Phase 2**（已交付） | 删除写内核 5 处显式索引写 + 测试基建改 `OpenTenant`（观察者装卷根，事件自动带 owner 前缀）+ R47 门禁 + R46 棘轮 4→0 | `TestService_SearchFiles_IndexTracks*`（upload/delete/rename/rmdir）在**无显式 upsert** 下仍绿；R47 变异（加回 `s.index.upsert`）红；删观察者安装 ⇒ 索引测试红 |
| **Phase 3**（已交付） | 全卷搜索：本地索引 + 外部卷经 `VolumeViewLister`（缓存）递归聚合；深度/条目/超时上限；失败隔离；tag 跳过外部 | 见 D4 验收 |

## 5. 风险与回滚

- `IsDir` 的 `Stat` 失败（目标已被并发删除）⇒ 保守 `false`（与现状一致，不引入新错误）；
- Phase 2 的最大风险是「某写面不发事件」⇒ 以「R47 + 契约测试 + E2E」三重兜底；回滚只需恢复
  显式调用（单文件 diff）；
- Phase 2 后写内核与索引彻底解耦：**新增写面若不经 `storage.Root`，索引不会更新**——由 R47 +
  契约测试兜底；新增本地写路径必须经 Root（这也是「按卷操作」纪律的一部分）。
- `RecordBypassWrite` 的 upsert 仍是外部卷唯一入口（无 Root 事件）；本地旁路写应改用
  `RecordBypassChecksum`（只补台账，索引由事件维护）。

## 6. 测试策略

- 单元：`ApplyRootMutation` 对 rename/remove 的 `IsDir` 分支（含源子树残留断言）；
- 集成：`dirsEnv` 真实 Root 事件链（Phase 2 后）；
- 变异：每个分支至少一个「改坏即红」的断言；
- 门禁：R47（写内核禁直接索引写）+ 既有 R41/R44/R46。
