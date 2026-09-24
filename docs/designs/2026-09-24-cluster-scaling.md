# 集群扩缩容管理（方案 A-⑤）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：配置模型 + 装配期校验 + 生命周期钩子
> 关联设计：[2026-09-24-statestore.md](./2026-09-24-statestore.md)（StateStore 承载节点元数据）、[2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（选主/下线释放租约）、[2026-09-24-cluster-index-consistency.md](./2026-09-24-cluster-index-consistency.md)（副本启动预热）

## 1. 背景与目标

### 1.1 现状

- 无节点生命周期管理：节点=进程，无身份、无角色概念；
- `pkg/server/config.go` 无集群段（仅 `hub.node_id` 等中继身份，与存储集群正交）；
- 多节点挂同一外部卷时：每节点各自为政（写面覆盖、索引不一致）——方案 A 的 LeaderElector/StateStore 解决了「写面唯一 + 状态一致」，但**谁来当节点、节点怎么加入/退出**没有管理面。

### 1.2 目标

- **扩容** = 加只读副本节点：新节点配置 `role: replica` + 挂同一外部卷 → 装配期校验 → 加入服务；
- **缩容** = 节点下线：主节点优雅释放 LeaderElector 租约 → 从节点补位；从节点直接退服；
- 节点身份（node_id）贯穿集群段、事件通道（write-coordination）、索引 Publish（index-consistency）三处；
- 单节点默认（不配 cluster 段）零回归。

### 1.3 非目标

- 不做自动化编排（k8s/consul 式自动扩缩——本期是「可配置 + 校验 + 生命周期钩子」，编排由外部负责）；
- 不做节点间状态迁移（副本加入时索引预热见 §2.4，但**不迁移文件本体**——外部共享卷是前提）；
- 不做多主（写面唯一由 LeaderElector 保证，本期不引入共识协议）。

## 2. 组件与接口

### 2.1 配置模型（`pkg/server/config.go` 新增 `ClusterConfig`）

```go
// ClusterConfig 是集群节点配置（cluster 段）。空 = 单节点（零回归）。
type ClusterConfig struct {
    Enabled    bool   `yaml:"enabled" mapstructure:"enabled"`
    NodeID     string `yaml:"node_id" mapstructure:"node_id"`     // 必填（Enabled 时）；跨节点唯一
    Role       string `yaml:"role" mapstructure:"role"`           // master | replica；默认 master
    ResyncInterval time.Duration `yaml:"index_resync_interval" mapstructure:"index_resync_interval"` // 索引 resync 兜底周期（默认 5m）
    // StorageRoot 提示校验：replica 必须挂外部卷（§2.3）；master 无强校验（单节点语义兼容）
}
```

- `SetDefaults`：`Enabled` 空 → false；`Role` 空 → master；`NodeID` 空 → 回落 `hub.node_id` → 再空 → 拒绝（Enabled 时）；
- `Validate`：`Enabled=true` 时 `NodeID` 必填、`Role ∈ {master, replica}`、`index_resync_interval >= 0`（0 = 关闭 resync）。

### 2.2 节点注册表（`pkg/state` 承载，仿 hub 节点表）

```
Key: nodes/<node_id>
Value: {role, addr, status: joining|active|draining|leaving, joined_at, last_seen}
```

- 装配期 `Put(nodes/<id>, joining)` → 校验通过 → `CAS(nodes/<id>, old=joining, new=active)`（**防同 id 双进程并发加入**）；
- 下线流程写 `draining` → Release 租约 → `leaving` → 保留 `last_seen`（诊断）；
- 存活心跳：周期 `CAS(nodes/<id>, ..., last_seen)` 更新（复用 StateStore 原子写；过期节点由 `/api/cluster/nodes` 展示为 stale，不自动清理——清理是运维动作，见 §4）。

### 2.3 装配期校验（fail-fast）

```go
// ValidateNode 装配期调用（cmd/sproxy/root.go 装配链，启动失败即拒绝）。
// replica 硬要求：
//   1. cluster.enabled=true；
//   2. 卷 root 与主节点同源（外部共享卷）：volume.Root 非本机默认 storage_root
//      （storage_root 为空或等于默认 ./storage → 拒绝：那是本机私有根）；
//   3. state_store.type != local（local 快照/元数据无跨节点共享语义）→ 拒绝；
//   4. leader.elector 可用（replica 也要能感知选主状态，只是不 TryAcquire 写租约——
//      或由 WriteGuard 装配但 isLeader 恒 false——**决策：replica 装配 WriteGuard，
//      TryAcquire 一旦成功即异常降级退出**，防「配置了 replica 却因 bug 抢到写」）。
// master 校验：cluster.enabled=true 时 state_store.type != local 同样拒绝
//   （多节点共享卷必须用 mongo，local 是单节点私有语义）。
```

- **零回归**：`cluster.enabled` 缺省 false → 全部校验跳过（单节点照旧，即使配了 state_store）。

### 2.4 节点生命周期（装配/退服钩子）

```
启动（replica）：
  config.Validate → NewLeaderElector → WriteGuard 装配（isLeader 恒 false）
  → 校验通过 → Put(nodes/<id>, joining) → CAS → active
  → [索引预热] List("index/") → 拉全部 owner 快照 → ReloadIndex（复用 index-consistency 的
     ReloadIndex，避免首搜全量 WalkDir——副本扩容后立即提供一致读，见 §7 预热说明）
  → 启动 HTTP 服务（只读面）

启动（master）：
  校验 → TryAcquire → 成功 → 主写面放行 → Put(nodes/<id>, active)
  → 失败（已被持有）→ 若配置 role=master → **启动失败**（fail-closed：显式主节点不得降级为从；
    文档明示多主应由运维先停旧主）——**决策**：master 配错才拒绝；想要从节点角色请显式配 replica。

下线（SIGTERM/优雅关闭，cmd/sproxy root.go 现有 shutdown 链扩展）：
  1. 停止接收新写（WriteGuard.Release 前先 isLeader=false——复用 leader-elector 的降级路径）；
  2. 存量请求排空（现有 graceful shutdown 超时语义）；
  3. 若持有租约 → LeaderElector.Release（从节点可立即补位，不等 TTL）；
  4. Put(nodes/<id>, leaving)（尽力而为，失败仅日志）；
  5. 退出。
```

- 崩溃（无优雅关闭）：flock/TTL 自动释放（leader-elector 语义）；`nodes/<id>` 残留 `active` 但 `last_seen` 过期 → `/api/cluster/nodes` 显示 stale（运维据 last_seen 判断）。

### 2.5 观测端点

- `GET /api/cluster/nodes`（受认证）：节点表（role/status/last_seen/持有租约的主节点 id——复用 leader-elector 的 `leader/global` 诊断 key）；
- `GET /api/cluster/self`：本节点 node_id/role/isLeader/state_store 类型（健康检查与排障用）。

## 3. 数据流

```
扩容（加副本）：
  运维挂同一外部卷 + 配置 cluster{enabled, node_id: r2, role: replica} + state_store{mongo}
  → 启动 → Validate（外部卷 + mongo + leader 装配）→ WriteGuard（isLeader=false）
  → Put(nodes/r2, joining) → CAS(active)
  → 索引预热 List("index/") → ReloadIndex 各 owner → 就绪（readyz 由 WriteGuard 状态参与）

缩容（下线主节点）：
  SIGTERM → isLeader=false（写面停）→ 排空 → Release 租约
  → 从节点 B 的 RenewLoop 退避重试 TryAcquire → 成功 → B 升主（写面放行）
  → B 成为 Publish 唯一执行者（index-consistency）→ Put(nodes/r2, leaving) → 退出

缩容（下线从节点）：
  SIGTERM → Put(nodes/rX, leaving) → 退出（不影响写面）

异常（主节点崩溃）：
  TTL 到期 → 从节点抢占 → 升主（leader-elector 既有语义）；nodes/<crash> 显 stale
```

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| replica 装配校验失败（本地卷/state_store=local） | 启动失败（fail-fast） | 防「以为副本实际各写各的」 |
| 同 node_id 双进程并发加入 | `CAS(joining→active)` 一方失败 → 启动失败 | 防身份冲突 |
| role=master 但租约被持有 | 启动失败（显式错误：另一主在线） | 防双主配置 |
| 下线时 Release 失败（mongo 不可达） | Warn + 继续退出（TTL 兜底） | 优雅路径尽力而为，异常路径由 TTL 收敛 |
| 下线时 Put(leaving) 失败 | Warn + 继续退出（last_seen 过期显 stale） | 同上 |
| replica 意外 TryAcquire 成功 | 立即降级退出（异常） | 防「副本抢写」的最坏情况 |

## 5. 测试 + 变异点

### 5.1 单元测试（`pkg/server` / `pkg/state`）

- `TestValidateClusterNode`：replica 用本地卷 → 拒绝；state_store=local → 拒绝；master 用 mongo → 通过；**变异：replica 本地卷放行 → 红**；
- `TestNodeRegistry_CASJoin`：并发同 id 加入 → 恰一个成功（**变异：CAS 退化为 Put 覆盖 → 红**）；
- `TestNodeRegistry_LifecycleStatus`：joining→active→draining→leaving 状态机非法跳转拒绝（**变异：状态机不校验 → 红**）；
- `TestShutdown_ReleasesLease`：fake elector → 优雅关闭调用 Release（**变异：关闭不 Release → 红**，从节点补位延迟断言）；
- `TestReplicaWriteGuard_AlwaysFollower`：replica 装配后 `Authorize()` 恒 503（**变异：replica TryAcquire 成功 → 红**）；
- `TestMasterRole_LeaseHeldStartupFails`：fake elector 已被持有 → 装配报错（**变异：降级为从继续启动 → 红**）。

### 5.2 集成

- 三节点（1 主 + 2 从，进程内，同 mongo 或同 StateStore 根）：下线主 → 从补位（条件轮询 isLeader 翻转）；`/api/cluster/nodes` 状态正确；
- 副本扩容后索引预热：主已 Publish 的 owner 在副本首搜即命中（免全量构建，断言构建计数 0——预热验证）；
- `127.0.0.1` 绑定铁律：所有测试监听 loopback。

### 5.3 门禁

- **R25 `cluster_config_gate_test.go`**（可选，仿 R15 docs_cli_flags）：断言 config.go 集群段必填字段与 docs/config.md 文档一致性（防配置漂移）；变异：删文档段 → 红。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **S1** | `ClusterConfig` 模型 + SetDefaults/Validate + 节点注册表（pkg/state `nodes/` key + CAS 加入） | 单测全绿 + 变异命中 | StateStore-F1、Leader-F1 |
| **S2** | 装配期校验链（replica 外部卷/state_store 检查、master 租约冲突 fail-fast）+ WriteGuard 角色接线 | 装配测试绿 + 变异命中 | S1、Leader-F2 |
| **S3** | 优雅下线流程（isLeader=false → 排空 → Release → leaving）+ 崩溃 stale 展示 + `/api/cluster/nodes`、`/api/cluster/self` | 集成测试绿（三节点补位） | S2 |
| **S4** | 副本索引预热（List("index/") → ReloadIndex）+ config.md 文档（R15） | 集成绿（预热断言）+ 文档门禁绿 | S3、index-consistency-C2 |

依赖图：S1 → S2 → S3 → S4；S4 依赖 index-consistency 的 ReloadIndex 入口。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 副本加节点后首搜全量 WalkDir 冲击外部卷 | 索引预热（S4）：启动即从 StateStore 拉快照，首搜免构建；预热失败不阻断启动（回退懒构建 + Warn） |
| replica 误抢写（bug/配置漂移） | 装配恒 503 + TryAcquire 成功即退出（双保险 fail-closed） |
| 双主配置（运维配两个 master） | master 启动遇租约被持有 → 响亮失败（不是降级） |
| node_id 冲突（同 id 双进程） | CAS joining→active 原子仲裁 |
| 下线顺序（先停写面后排空）破坏在途请求 | 复用 leader-elector 降级路径 + 现有 graceful shutdown 排空语义 |
| 外部卷判定误判（本地卷挂 NFS 被当外部卷） | 文档明示判定口径：`volume.Root` 与默认 `./storage` 不同即视为外部卷候选；精确「是否共享」由运维保证（同 leader-elector 的同目录文件系统前提） |

**零回归保证清单**：
1. `cluster.enabled` 缺省 false → 校验/注册表/预热/下线钩子全部不装配（单节点行为逐字不变）；
2. NodeID 缺省回落 `hub.node_id`（已有身份优先复用）；
3. 下线流程复用既有 graceful shutdown 链（只加钩子不改语义）；
4. 预热失败不影响启动（懒构建兜底，与现状首搜构建语义一致）；
5. 新端点（/api/cluster/*）受既有认证中间件保护，默认不暴露额外信息。

## 8. 关联项

- LeaderElector：`Release` 是缩容的核心动作；`leader/global` 诊断 key 供 `/api/cluster/nodes` 展示当前主；
- StateStore：`nodes/<id>` 注册表 + CAS 加入 + `last_seen` 心跳；
- index-consistency：副本预热复用 `ReloadIndex`；master 换主后 Publish 唯一执行者切换；
- write-coordination：事件通道 node_id 字段来源即本配置（同一身份贯穿）。
