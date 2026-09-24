# 集群索引一致性（方案 A-④）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：`pkg/files` 可选注入 + `pkg/server` 装配层
> 关联设计：[2026-09-24-statestore.md](./2026-09-24-statestore.md)（StateStore 承载快照 + Watch）、[2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（谁有权 Publish）、[2026-09-24-cluster-write-coordination.md](./2026-09-24-cluster-write-coordination.md)（事件通道互补）

## 1. 背景与目标

### 1.1 现状

- `pkg/files/search_index.go`：按 owner 分片内存索引，**进程内** copy-on-write（持锁深拷贝→替换指针），写路径增量 upsert/remove/rename；
- `pkg/files/index_persist.go`：快照落 `<tenant meta>/index/<owner>.json`（tmp+rename），`ensureOwner` 先载快照再全量 WalkDir 构建；`invalidate(owner)` 删内存+删快照；
- **多节点挂同一外部卷时不一致**：主节点写路径增量只改本进程内存；各节点快照互相覆盖（同路径同名文件）；副本 `built[owner]=true` 后永不重读快照 → 索引过期。

### 1.2 目标

- 主节点索引更新 → `Put(index/<owner>)`（StateStore）+ 事件广播；
- 副本 `Watch("index/")` → 收到变更 → **重载**该 owner 索引（快照载入优先，全量重建兜底）；
- 单节点（未装配集群）零回归：快照路径、载入语义与现状逐字一致。

### 1.3 非目标

- 不做实时读一致性保证（最终一致，窗口 ≤ 信号延迟）；
- 不做快照增量传输（全量快照足够：单 owner 索引是目录条目集合，量级小）；
- 不把索引快照纳入 Raft 复制（StateStore 承载即够，Watch 信号驱动失效）。

## 2. 组件与接口

### 2.1 快照信封（新，`pkg/files/index_sync.go`）

```go
// indexEnvelope 是 StateStore 承载的快照信封（index_persist.go 的 DTO 外包一层）。
// rev 由主节点 per-owner 单调递增（启动置 1，每次 Publish +1）；node 是发布者标识。
// 副本用 rev 去重/防乱序：旧 rev 信封到达必须忽略（不覆盖新索引）。
type indexEnvelope struct {
    Rev     int64                          `json:"rev"`
    Node    string                         `json:"node"`
    Updated int64                          `json:"updated_at"` // UnixNano，诊断用
    Entries map[string]*indexSnapshotEntry `json:"entries"`
}
```

- 复用 `indexSnapshotEntry`/`fromSnapshotEntries`（索引条目 DTO 不动，零回归）；
- `contentTokens` 仍不入快照（与现状一致：快照载入后无内容词元，全量重建才有——语义保持）。

### 2.2 领域包可选注入（`pkg/files` 能力接口，仿「接口不动、装配层适配」策略）

```go
// IndexSync 是集群装配层注入的可选钩子（searchIndex 上挂 nil = 零回归）。
type IndexSync interface {
    // Publish 由主节点写路径增量后调用：rev++ → StateStore.Put(index/<owner>, envelope)。
    Publish(ctx context.Context, owner string, entries map[string]*indexEntry) error
    // Load 由副本重载时调用：从 StateStore Get(index/<owner>) 读信封（不存在 → ErrKeyNotFound）。
    Load(ctx context.Context, owner string) (*indexEnvelope, error)
}
```

- `Service` 新增 `AttachIndexSync(IndexSync)`（构造后装配期调用，nil 不装配）；
- `Service.ReloadIndex(owner string, env *indexEnvelope)`：**校验 rev > 本进程已应用 rev**（`appliedRev map[owner]int64`）→ 快照载入替换 owner 索引（复用 clone/替换指针语义，`built=true` 保持）→ 失败/损坏 → 回退 `InvalidateIndex`（现有全量重建路径）；
- `Publish` 触发点：`saveAll()` 成功路径 + `ensureOwner` 全量构建后（主节点）；写路径增量（upsert/remove/rename）**不逐个 Publish**（高频低收益），由周期 `saveAll` 或事件驱动的显式 Publish 承担——**决策：写路径增量后置 dirty 标记，saveAll 周期（IndexSaveInterval）统一 Publish**；事件驱动的即时 Publish 见 write-coordination 设计（主节点收到本节点写事件后 Publish，低延迟可选）。

### 2.3 副本 Watch 循环（装配层，`pkg/server`）

```go
// IndexSyncLoop 订阅 StateStore.Watch("index/")，收到 put → Load → ReloadIndex。
// 事件通道（write-coordination）与 Watch 共用同一处理器（rev 幂等去重）。
func (h *Handlers) indexSyncLoop(ctx context.Context) { ... } // stop channel + WaitGroup，同 cleanupUploadingFilesLoop 模式
```

- 复用 `WatchStore.Watch` 约定：通道关闭 → 指数退避重连（1s→2s→4s→封顶 10s）；
- **resync 兜底**：每 `cluster.index_resync_interval`（默认 5m）List("index/") 比对各 owner 本地 appliedRev，落后 → 重载（覆盖「事件/Watch 均丢失」的窗口）；
- 事件通道（/api/events）与 Watch 同时启用时：同一 `ReloadIndex` 入口，rev 比较天然幂等（重复信号只触发一次真实载入）。

## 3. 数据流

```
主节点写路径成功（upload/rename/delete/restore/...）
  → searchIndex.upsert/remove/rename（现状，内存态先行）
  → dirty[owner]=true
  → saveAll 周期到（IndexSaveInterval，默认 5m；或事件驱动即时 Publish）
      → IndexSync.Publish：rev++ → StateStore.Put("index/<owner>", envelope)
      → [集群态] 跳过本地 <tenant meta>/index/<owner>.json 落盘（双轨切换，见 §7）
  → 事件广播（write-coordination：/api/events file.index.update{owner, rev}）

副本
  Watch("index/") → Change{Key:"index/<owner>", Op:"put"}
    → Load(index/<owner>) → 校验 rev > appliedRev[owner]
      → ReloadIndex(owner, env)（快照载入替换指针，built 保持）
      → 损坏/Get 失败 → InvalidateIndex(owner)（全量重建，fail-safe）
  /api/events 收到 file.index.update（若启用）→ 同一 ReloadIndex 入口（rev 幂等）
  resync 周期（5m）→ List("index/") 比对 rev → 落后则重载
```

**顺序保证**：业务落盘（文件+台账）→ PublishIndex → 事件。副本任何信号到达都读 StateStore 快照（rev 校验），故信号顺序无关紧要——一致性事实源是 `index/<owner>` 快照本身。

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| Publish 失败（mongo 不可达/磁盘满） | Warn 日志 + dirty 保持（下周期重试） | 索引是加速层（现状快照失败仅日志），读一致性降级由副本「读旧快照」兜底 |
| Watch 通道关闭 | 退避重连（1s→10s 封顶）+ 日志 | 复用 WatchStore 约定；resync 兜底覆盖断连窗口 |
| Get 失败 / 快照损坏 | 日志 + `InvalidateIndex`（全量重建） | fail-safe：宁可重扫不可用错索引 |
| 旧 rev 信封乱序到达 | 忽略（不覆盖） | rev 幂等是核心不变式 |
| 主节点 `ErrLeaseLost` 降级 | 停止 Publish（写面已停，天然停止） | 与 leader-elector 衔接：非主不发布 |
| 单节点（IndexSync nil） | 全部新逻辑不装配 | 零回归 |

## 5. 测试 + 变异点

### 5.1 单元测试（`pkg/files`）

- `TestIndexEnvelope_RoundTrip`：信封 JSON 序列化/反序列化 + 与 `indexSnapshotFile` DTO 互转兼容；
- `TestReloadIndex_AppliesNewerRev`：rev 递增载入 → search/list 命中新条目（断言与全量构建等价）；**变异：ReloadIndex 不走 rev 比较直接覆盖 → 红**；
- `TestReloadIndex_StaleRevIgnored`：旧 rev 到达 → 索引保持（**变异：删 rev 检查 → 红**）；
- `TestReloadIndex_CorruptFallbackToRebuild`：损坏快照 → 回退 `InvalidateIndex` 全量重建（**变异：损坏时返回空索引 → 红**）；
- `TestAttachIndexSync_NilNoop`：未装配时 saveAll/ensureOwner 行为与现状逐字一致（零回归断言）；
- `TestPublish_DirtyTracking`：写路径增量置 dirty → saveAll 只 Publish dirty owner（**变异：全量 Publish → 红**，防重载风暴）。

### 5.2 装配层（`pkg/server`）

- `TestIndexSyncLoop_ReloadOnChange`：mock WatchStore 推 put → 断言 ReloadIndex 被调（fake service 探针）；**变异：收到 Change 不处理 → 红**；
- `TestIndexSyncLoop_ReconnectAfterClose`：通道关闭 → 重连后重推仍生效（**变异：断连不重推 → 红**）；
- `TestResyncLoop_CatchesUp`：落后 rev 的 owner 被重载（**变异：resync 不跑 → 红**）。

### 5.3 集成

- 双节点共享卷（进程内两个 Handlers，同 StateStore 根，一主一从）：主上传 → 从 search 命中（条件轮询 `testutil.WaitFor`，不 `time.Sleep`——R14 棘轮）；
- 事件通道（真实 SSE 或内存传输）→ 副本失效 → 一致（同断言）。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **C1** | 信封 + `IndexSync` 接口 + `AttachIndexSync` + `ReloadIndex`（rev 校验/回退重建）+ dirty 跟踪 | 单测全绿 + 变异命中 | StateStore-F1 |
| **C2** | 主节点 Publish 接线（saveAll 周期 + 集群态跳过本地快照）+ 双轨切换 | 集成测试绿（主写从读一致） | C1、Leader-F2（仅主 Publish） |
| **C3** | 副本 `IndexSyncLoop`（Watch + 退避重连 + resync 兜底）+ 事件通道适配（write-coordination 共用入口） | 装配测试绿 + 变异命中 | C1、C2、write-coordination-C1 |
| **C4** | 双节点集成测试 + docs（config.md `cluster.index_resync_interval` 段，R15） | 集成绿 + 文档门禁绿 | C3 |

依赖图：StateStore-F1 → C1 → C2 → C3 → C4；C3 与 write-coordination 的副本侧共用 `ReloadIndex` 入口。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 快照双轨（本地 meta 路径 vs StateStore）切换引入回滚困难 | 单节点（IndexSync nil）恒走 `<tenant meta>/index/<owner>.json` 原路径；仅 `cluster` 装配（role 非空 + StateStore 非 local）时切换 StateStore 承载；关掉装配段即回滚 |
| 副本重载风暴（多 owner 同时失效） | 快照载入（非全量 WalkDir）为默认重载路径；dirty 只 Publish 变更 owner；rev 幂等防重复 |
| 事件/Watch 双通道信号重复 | 统一 `ReloadIndex` 入口 + rev 比较，天然幂等 |
| 主节点 rev 计数器重启归零 | rev 与 node 一起入信封；副本按 `(node, rev)` 单调比较——同 node 才比 rev，跨 node 一律重载（换主即全量对齐一次，安全方向） |
| Watch 轮询开销（local 实现） | 默认间隔 500ms + resync 5m 兜底；事件通道提供低延迟路径 |

**零回归保证清单**：
1. 未装配 `IndexSync`（单节点默认）→ searchIndex 行为、快照路径、落盘语义与现状逐字一致；
2. `indexSnapshotEntry` DTO 与 `index_persist.go` 载入路径不改（信封复用同一 DTO）；
3. contentTokens 不入快照的现状语义保持；
4. 副本重载用 copy-on-write 整表替换（复用既有并发模型），不破坏在途 reader；
5. 快照失败仅日志（加速层语义）保持。

## 8. 关联项

- StateStore：`index/<owner>` key 与 §2.3 key 规范一致；Watch 约定（初始不重放、通道关闭语义）直接复用；
- LeaderElector：仅主节点 Publish（`WriteGuard.isLeader` 判定）；`ErrLeaseLost` 后 Publish 自然停止；
- write-coordination：事件通道是 Watch 的互补低延迟路径，共用 `ReloadIndex` 入口；
- 后续片：快照增量传输（大 owner 优化）、副本启动预热（从 StateStore 拉全量快照，免首搜全量构建）。
