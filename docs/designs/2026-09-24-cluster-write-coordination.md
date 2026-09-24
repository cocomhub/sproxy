# 集群写面协调（方案 A-⑥）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：事件通道适配 + 写 handler 接线 + 副本失效
> 关联设计：[2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（写面门）、[2026-09-24-cluster-index-consistency.md](./2026-09-24-cluster-index-consistency.md)（索引失效共用入口）、[2026-09-24-statestore.md](./2026-09-24-statestore.md)（事件持久化）

## 1. 背景与目标

### 1.1 现状

- 主节点写 → 无广播：`pkg/server/handlers_endpoints.go` 只含基础端点与自愈循环（livez/readyz/cleanup 等），**不存在 /api/events 事件流**——任务简报所述「已有」在现源码中未发现，需新建；
- 副本不知道文件变了：读面（search/list/download）持续返回旧数据（文件本体可见但索引/缓存未失效）。

### 1.2 目标

- 主节点写 handler 成功 → 事件总线发 `file.change`（owner/rel/op/rev）→ 副本订阅 → **索引失效 + 缓存失效**；
- 事件通道成为「低延迟失效信号」，与 StateStore.Watch（快照重载）互补：事件负责「及时」，Watch/resync 负责「兜底」；
- 未装配集群（单节点）零回归：事件总线不装配、写路径无额外开销。

### 1.3 非目标

- 不做事件溯源（event sourcing）——事件仅作失效信号，事实源仍是 StateStore 快照/文件本体；
- 不做「副本读转发到主」的一致读（写面转发属后续片，leader-elector 已明示）；
- 不保证事件至少一次投递——**失效是幂等操作**（重载一次/多次结果相同），丢事件由 resync 兜底。

## 2. 组件与接口

### 2.1 事件模型（`pkg/server` 内新建 `events.go`，仿 audit/notify 同包模式）

```go
// FileChangeEvent 是写面变更事件（事件总线消息体）。
type FileChangeEvent struct {
    Op    string `json:"op"`              // upload | delete | rename | mkdir | rmdir | restore | version_gc
    Owner string `json:"owner"`
    Rel   string `json:"rel"`             // 相对 user 桶路径（ToSlash）
    To    string `json:"to,omitempty"`    // rename 目标
    Rev   int64  `json:"rev"`             // 主节点 per-owner 索引 rev（与 index-consistency 信封一致）
    Node  string `json:"node"`            // 发布者 node_id（cluster.node_id）
    TS    int64  `json:"ts"`              // UnixNano，诊断/去旧
}

// EventBus 是进程内事件总线（装配层可选注入；nil = 零回归）。
// Publish 同步发往已订阅的消费者（回调式）；持久化由 StateStore.AppendStore 承接（可选）。
type EventBus struct {
    mu       sync.RWMutex
    handlers map[string][]func(FileChangeEvent) // key = 事件类型前缀（"file." 等）
}
func (b *EventBus) Subscribe(prefix string, fn func(FileChangeEvent)) (cancel func())
func (b *EventBus) Publish(evt FileChangeEvent) // 同步回调；handler panic 由 recover 捕获（不扩散）
```

- **注意**：任务简报提到「/api/events 事件流已有」——经核实现源码无此端点，故本设计把「进程内 EventBus + 可选 SSE 端点」一并纳入（§2.3 的 SSE 是新端点，受认证保护）。

### 2.2 写 handler 接线（`pkg/server` 装配）

- `Handlers` 新增 `eventBus *EventBus`（nil = 未装配）+ `fileEventRecorder func(FileChangeEvent)`（写路径调用点注入，避免 Handlers 大改）；
- 写面成功路径调用点（与 WriteGuard 门同清单）：
  - `POST /upload`、分块 `POST /upload/complete`（init/chunk 不触发）；
  - `POST /delete`、`POST /rename`、`POST /mkdir`、`POST /rmdir`；
  - `POST /api/versions/restore`、`POST /api/archive`（解压落盘）、云下载任务完成落盘（cloud_downloader 完成钩子，可选 P1）；
  - 版本 GC/回收站 GC（周期后台写，发 `version_gc`/`trash_gc` 事件——副本只需失效对应 owner 索引）；
- **顺序**：业务成功（文件+台账落盘）→ `file.change` → 索引 Publish（index-consistency）。事件在前还是后不影响正确性（副本收到事件只标记 dirty，重载时读快照）。

### 2.3 传播通道（两种，可同时启用）

```
通道 A（StateStore Watch，兜底）：
  主：Publish 后 Put(index/<owner>)（index-consistency C2）
  副本：Watch("index/") → ReloadIndex（index-consistency C3）——事件到达前快照已更新，
       副本重载读到的即最新；**本通道已足够保证最终一致**。

通道 B（SSE 事件流，低延迟补充，新端点）：
  主：EventBus.Publish → SSE 广播（GET /api/events?type=file.change，受认证）
  副本：SSE 客户端长连接 → 收到 file.change → 立即 ReloadIndex（rev 幂等）
  —— 目的：比 Watch 轮询（local 500ms / mongo change streams 延迟）更快触发副本失效。
```

- **决策**：通道 B 是可选增强（`cluster.events_sse: true` 显式开启，默认关零回归）；通道 A 是默认一致性路径。任务简报「变更经 /api/events 广播」按通道 B 落地，但一致性事实源仍在 StateStore 快照。

### 2.4 缓存失效（副本读面）

- 索引：`ReloadIndex(owner)`（index-consistency 同一入口，rev 幂等）；
- 其它读缓存：本期仅索引（现状无其它文件元缓存）；未来配额/分享计数缓存失效沿用同一 EventBus 前缀订阅（`share.`/`quota.` 预留）；
- 文件本体缓存（若有 OS page cache）不在应用层控制范围，文档说明。

## 3. 数据流

```
主节点 POST /upload 成功
  → 业务落盘（文件 + checksum 台账 + 索引增量 upsert，现状）
  → EventBus.Publish(FileChangeEvent{op:upload, owner, rel, rev})
      → [通道 B] SSE 广播（若 events_sse 开启）
      → [通道 A] 同线程内继续：IndexSync.Publish → StateStore.Put(index/<owner>, envelope)
  → 200 响应

副本：
  [通道 A] Watch("index/") → put → Load → ReloadIndex（rev 校验）
  [通道 B] SSE 收到 file.change → ReloadIndex（rev 幂等；若快照尚未 Put 完 → 重载读旧
           快照 → 落后 → 由 resync 周期兜底补齐）
  → 后续 search/list 命中新数据

网络分区/事件丢失：
  → resync 周期（index_consistency C3，5m）List("index/") 比对 rev → 落后重载 → 收敛
```

**不变式**：副本任何路径（Watch/SSE/resync/预热）都收敛到「读 StateStore 快照 + rev 校验」单一入口——事件只决定「何时去检查」，不决定「读到什么」。

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| Publish 时 handler panic | recover + 日志（不扩散到写路径） | 事件是尽力而为（同审计语义：失败不阻断业务） |
| SSE 客户端断开 | 清理订阅（cancel）+ 重连由客户端负责 | 通道 B 是增强，断连不损害一致性（通道 A/resync 兜底） |
| 事件先于快照 Put 到达（竞态） | ReloadIndex 读旧快照 → rev 落后 → 忽略或落后载入 → resync 补齐 | 幂等 + 兜底闭环 |
| 未装配 EventBus（单节点） | 写路径零额外调用（nil 判断） | 零回归 |
| 事件持久化（AppendStore）失败 | Warn + 继续（尽力而为） | 事实源是快照，事件不持久化也不损失一致性 |
| 副本收到 rename 事件但快照缺失 | InvalidateIndex（全量重建） | fail-safe |

## 5. 测试 + 变异点

### 5.1 单元测试（`pkg/server`）

- `TestEventBus_PublishSubscribe`：订阅前缀命中/不命中、取消订阅、handler panic 被 recover（**变异：panic 扩散 → 红**）；
- `TestWriteHandler_PublishesFileChange`：fake recorder → 上传成功后收到 `{op:upload, owner, rel}`（**变异：成功路径不 Publish → 红**）；
- `TestWriteHandler_NoEventBus_NilNoop`：未装配 → 写路径行为与现状逐字一致（零回归断言）；
- `TestWriteHandler_Delete_Rename_Events`：delete/rename/mkdir/rmdir 各自触发对应 op（**变异：漏 delete 事件 → 红**）；
- `TestReadHandlers_NoPublish`：GET/HEAD/search/list/download 不产生事件（**变异：读面 Publish → 红**）；
- `TestSSEEndpoint_AuthAndFilter`：未认证 401、type 过滤、连接断开清理（**变异：断开不清理订阅 → 红**）。

### 5.2 集成（双节点进程内，同 StateStore 根）

- 主上传 → 副本 SSE 收到 `file.change` → 副本 search 命中（条件轮询 `testutil.WaitFor`，不 `time.Sleep`——R14）；
- **事件丢失模拟**：SSE 通道关闭（不装配通道 B）→ 副本仅靠 Watch/resync 收敛（轮询断言最终一致——**核心验收：不依赖事件通道也能收敛**）；
- **事件先于快照竞态模拟**：手动先发事件后 Publish → resync 兜底补齐（断言最终一致）。

### 5.3 变异点汇总

| 变异 | 预期红 |
|---|---|
| 写成功不 Publish | TestWriteHandler_PublishesFileChange |
| 读面也 Publish | TestReadHandlers_NoPublish |
| 事件丢失无 resync 兜底（删 resync 循环） | 集成「事件丢失模拟」 |
| 副本收到事件不触发失效 | TestIndexSyncLoop_ReloadOnChange（index-consistency） |
| SSE 无认证 | TestSSEEndpoint_AuthAndFilter |

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **W1** | `FileChangeEvent` 模型 + `EventBus`（订阅/发布/recover）+ 写 handler 调用点接线（upload/complete/delete/rename/mkdir/rmdir/restore） | 单测全绿 + 变异命中 | 无（独立于 StateStore） |
| **W2** | SSE 端点 `GET /api/events`（认证 + type 过滤 + 断连清理）+ `cluster.events_sse` 配置 | 单测绿 + 认证断言 | W1、cluster 配置段（scaling-S1） |
| **W3** | 副本 SSE 客户端适配（EventBus 订阅 → ReloadIndex 桥接）+ 与 Watch/resync 的 rev 幂等合并 | 集成测试绿（双通道收敛 + 竞态模拟） | W1、W2、index-consistency-C3 |
| **W4** | 后台写事件（版本 GC/回收站 GC/云下载完成）+ docs/config.md 文档（R15） | 单测绿 + 文档门禁绿 | W1 |

依赖图：W1 → W2 → W3；W3 与 index-consistency-C3 共用 ReloadIndex 入口；W4 ∥ W3。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 事件丢失导致副本长期不一致 | 一致性事实源在 StateStore 快照 + resync 周期兜底（5m）——事件只缩短窗口，不承担正确性 |
| 事件风暴（高频写） | 副本只做 rev 校验 + 载入（快照载入 O(entries)）；resync 比对用 rev 不全量拉；SSE 可配过滤（type=file.change 已是窄集） |
| 写路径 Publish 阻塞业务 | EventBus 同步回调但 handler 轻量（只入队标记 + SSE 扇出）；SSE 扇出失败（慢消费者）→ 丢弃 + Warn（可观测），不阻塞写面 |
| 事件/快照顺序竞态 | rev 幂等 + resync 兜底闭环（§3 不变式） |
| 新端点暴露信息面 | `GET /api/events` 受既有 authMiddleware 保护；事件体无敏感内容（owner/rel 是用户本可见数据） |

**零回归保证清单**：
1. 未装配 EventBus（单节点默认）→ 写路径无任何额外调用（nil 判断先行）；
2. `events_sse` 默认关 → 无新监听/新端点暴露；
3. 事件发布在业务成功后（不改变写路径语义与错误返回）；
4. 副本失效统一走 ReloadIndex（rev 幂等），不新增旁路逻辑；
5. 尽力而为语义（同审计：失败记日志不阻断业务）文档化。

## 8. 关联项

- LeaderElector：写 handler 在 `WriteGuard.Authorize()` 之后、业务之前接 Publish（仅主节点到达此处）；
- index-consistency：`rev` 字段与信封 rev 同源（主节点 per-owner 计数器）；ReloadIndex 为唯一失效入口；
- scaling：事件 `node` 字段 = `cluster.node_id`；SSE 配置并入 `cluster` 段；
- StateStore：可选 AppendStore 持久化事件（审计/排障），P2 片。
