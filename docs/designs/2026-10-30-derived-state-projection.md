# 派生状态投影：单一写入口与事件语义（设计）

> 状态：Phase 1 实施中（2026-10-30）。范围：搜索索引（`pkg/files`）、卷状态缓存
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

### D2 单一投影器（Phase 2，下一片）

- 新增 `pkg/files` 内的 `derivedProjector`（或 `pkg/derived`），唯一职责：把一次 `Mutation` 投影到
  索引 + （经装配层）缓存失效 + 台账变更；
- 删除 `write_ops.go` 的 5 处显式索引调用与 `recordUploadSuccess` 的 upsert；`tags.go` 的
  `setTags` 保留（标签不是 Root 事件，属显式写面）；
- 目录 rename 的子树搬移由投影器完成：`removePrefix(from)` → 递归 `ReadDir` 目标子树 → `upsert*`；
- **前置**：测试基建（`dirsEnv`）的写路径统一走 `storage.Root`（当前部分用例以 os 直写，
  掩盖了事件是否真正覆盖）；否则无法用测试证明 I1。

### D3 快照安全（Phase 1：已核实，**无需代码变更**）

原假设「索引快照会明文落节点磁盘」经核实**不成立**：

- `saveIndexSnapshot` 经 `storage.Root.OpenFileEncrypted` 写（`meta/index/<owner>.json` 在**卷内**）
  ⇒ 加密卷上快照内容为密文；
- `volumestate.Store` 的 `persist_private` 是另一回事：它落**节点本地明文目录**（非卷根），
  所以需要按卷关闭；索引快照不在此列。

⇒ 保持现状（零代码变更），避免引入「所有本地卷都被判私密 ⇒ 索引永不落盘」的错误判据
（`volumePrivate` 对 `DirectLink=false` 的本地卷返回 true，直接复用会让索引退化为每次重启全量扫描）。

### D4 外部卷搜索（Phase 3，待产品口径）

当前搜索只覆盖有本地根的卷（与默认 list 聚合一致）。支持外部卷需要递归列举远端目录
（每次查询 O(目录数) 远端往返），与「索引加速」目标冲突。设计取舍：

- 选项 A（推荐）：**不纳入**搜索，文档明确；显式 `?volume=` 列表已覆盖「浏览外部卷」；
- 选项 B：为外部卷建**独立内存索引**（远端列举后缓存 + TTL/事件失效），仅当用户显式请求
  `?volume=<外部卷>` 时启用；
- 选项 C：查询时实时递归（无索引）——成本最高，仅在结果集很小时可接受。

本设计**不实施**，等产品口径；`docs/api.md` 已写明覆盖范围。

## 4. 分期与验收

| 阶段 | 内容 | 验收（变异必须命中） |
|---|---|---|
| **Phase 1**（本片） | `Mutation.IsDir`（Root 操作前 Stat 判定）+ 目录 rename 全投影（删源前缀 + 按磁盘重建目标子树）+ 删除写内核显式 `index.rename` + R45 门禁接受 `notifyMutationKind` | 目录 rename 后源前缀条目消失且目标子树路径正确（变异：改回单条 ⇒ 「应命中 z/b/keep.txt，got a/b/keep.txt」红）；`IsDir` 语义（Mkdir/RemoveAll true、文件 write/rename/remove false、目录 rename/remove true） |
| Phase 2 | 去双写：投影器唯一入口 + 测试基建统一 Root + 目录 rename 目标子树递归重建 + R47 门禁 | 删除显式 `index.upsert` 后上传/删除/rename 契约测试仍绿；R47 源码扫描红 |
| Phase 3 | 外部卷搜索（按产品口径） | 按选定选项 |

## 5. 风险与回滚

- `IsDir` 的 `Stat` 失败（目标已被并发删除）⇒ 保守 `false`（与现状一致，不引入新错误）；
- Phase 2 的最大风险是「某写面不发事件」⇒ 以「R47 + 契约测试 + E2E」三重兜底；回滚只需恢复
  显式调用（单文件 diff）；
- Phase 1 删除 `index.rename` 后，目录 rename 的索引维护完全依赖事件；若某写面不经 `Root`
  （当前仅测试基建的 os 直写），索引会滞后 ⇒ Phase 2 一并统一测试基建并加 R47 门禁。

## 6. 测试策略

- 单元：`ApplyRootMutation` 对 rename/remove 的 `IsDir` 分支（含源子树残留断言）；
- 集成：`dirsEnv` 真实 Root 事件链（Phase 2 后）；
- 变异：每个分支至少一个「改坏即红」的断言；
- 门禁：R47（写内核禁直接索引写）+ 既有 R41/R44/R46。
