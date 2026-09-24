# 集群状态上移（11.11-②）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：装配层适配 + 集群模式开关
> 关联：[2026-09-24-statestore.md](./2026-09-24-statestore.md)（StateStore 抽象与迁移矩阵 F2）、
> [2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（WriteGuard 写面唯一）
> **本任务是 statestore.md 迁移矩阵在集群场景的装配路径**：哪些 Store 集群模式必迁、如何切、如何保证零回归。

## 1. 背景与目标

### 1.1 现状（源码实证）

| 状态 | 源码 | 落盘 |
|---|---|---|
| 凭据 Ring | `pkg/accesskey/storer.go`（CredentialStorer.Load/Save）+ `pkg/server/credentials.go`（BootstrapServerCredentials） | `<默认卷根>/anonymous/meta/credentials.json`（AD-5：meta 归属默认卷不变式） |
| dedup 台账 | `pkg/files/dedup.go`（DedupStore：checksum→refs，saveMu+tmp+rename+Windows 回退+失败重试一次） | `<meta>/dedup.json` |
| 索引快照 | `pkg/files/search_index.go`（searchIndex 内存 copy-on-write；ensureOwner/saveAll 落盘快照） | `<meta>/index/<owner>.json` |
| 分享 | `pkg/server/share.go`（statestore.md §1.1 已列） | `<share>/<token>.json` |

多节点挂同一外部卷：各节点各写各的本地 JSON → 静默互相覆盖（现状无写面仲裁）。

### 1.2 目标

- 集群模式（`state_store.type != local`，或 local 但共享外部卷 + 显式 `cluster.enabled: true`）下，
  凭据 / dedup / 分享 / 索引快照 **强制**切 StateStore（主写副本读）；
- **配额不迁 StateStore**（高频内存账本，statestore.md §5.1 P2 决策）：跨节点一致性由 LeaderElector 写面唯一承担，本设计重申并文档化；
- 未装配 StateStore（type=local 单节点）→ 全部 Store 走原实现（零回归）。

### 1.3 非目标

- 不做 local→mongo 数据搬运（运维外部完成；读旧格式兼容由适配器承担）；
- 不做跨节点数据迁移/合并工具；
- 不做副本节点写转发（见只读副本设计）。

## 2. 组件与接口

### 2.1 集群模式判定（装配层 `cmd/sproxy/root.go`）

```go
// clusterMode 派生规则：
//   state_store.type == mongo                       → 集群（集中 DB 形态）
//   state_store.type == local && cluster.enabled    → 集群（共享外部卷形态，state dir 指外部卷）
//   其余                                             → 单节点（零回归）
// 集群模式 + StateStore 装配失败 → 启动失败（fail-closed：防「以为多节点一致、实际各写各的」）。
```

### 2.2 适配器（实现既有接口、内部委托 StateStore；领域包接口不动）

- **`StateBackedCredentialStorer`**（实现 `accesskey.CredentialStorer`）：
  - `Load` → `Get("credential/anonymous/ring")`；`ErrKeyNotFound` → `(nil, nil)`（对齐零凭据启动 U3）；
    值损坏 → error（fail-closed，对齐 credentials.go `Load` 的 panic 语义）；
  - `Save` → `Put` 全量快照（序列化格式与 `credentialstore.go` 一致）；
  - 装配点：`BootstrapServerCredentials` 的 store 构造处替换（credentials.go `accesskey.NewCredentialStore(metaDir)` 分支）；
  - **加密链保留**：`EncryptingStorer`/`SecureStorer` 包装在 StateStore 之上（StateStore 值已是密文），Vault/aesgcm 语义不变。
- **`DedupStore` 可选 state 后端**（`pkg/files/dedup.go` 增加 `state StateStore` + `stateKey string` 字段，nil = 本地 JSON 零回归）：
  - key = `dedup/<owner>/all` 单 key 全量快照（statestore.md §5.1 矩阵）；
  - `save()` → `state.Put`；`NewDedupStore` 加载：state.Get 优先、旧 `meta/dedup.json` 回退（双读单写）；
  - 引用计数跨节点强一致：本期靠「写面唯一（LeaderElector）」已够——单 key 快照 + 只有主节点写，无 CAS 争抢；逐 checksum CAS 留作 P1 演进。
- **分享适配器**（`pkg/server/share.go`）：key = `share/<token>` 逐条；Consume 计数走 CAS 重试循环（statestore.md §3.2 同款）。
- **索引快照后端**（装配层注入 searchIndex 的 load/save 函数）：key = `index/<owner>` 单 key；
  `saveAll` → Put；`ensureOwner` → Get 优先、缺失回退本地 WalkDir（副本读共享卷文件本体可自建）。

### 2.3 配置

```yaml
state_store: ...        # statestore.md §5.3（type/dir/mongo.*）
cluster:
  enabled: false        # 默认 false（零回归）；true = 共享外部卷形态的集群
  mode: primary         # primary=主写面 | replica=只读副本（见只读副本设计）
```

`SetDefaults`：`cluster.enabled` 空 → false。`Validate`：`cluster.mode=replica` 时 `cluster.enabled` 自动视为 true。

## 3. 数据流

### 3.1 凭据（主写副本读）

```
主节点 register/renew/rotate → Ring.Replace → StateBackedCredentialStorer.Save → Put(credential/anonymous/ring)
副本节点启动 bootstrapCredentials → Load → Get → Ring 内存副本
  （读面 SproxySig 校验需要 AK→SK 查表——副本必须载入同一 Ring，否则 401）
```

### 3.2 dedup 台账

```
主节点 upload → DedupStore.Add/RemoveRef → save() → Put(dedup/<owner>/all)
副本节点读面（list/stat 带 checksum）→ checksum 台账同款后端 → Get
```

### 3.3 索引快照 + 失效广播

```
主节点写路径增量 + 周期 saveAll → Put(index/<owner>)
副本节点 ensureOwner → Get(index/<owner>) 优先载入 → 免 WalkDir；缺失 → WalkDir 共享卷全量构建
  （快照是加速层：Get 失败不阻断读，日志告警；Watch 失效广播为后续片，statestore.md §3.4）
```

### 3.4 分享

```
主节点 POST /api/share → CAS(share/<token>, old=nil, new=...) 创建
消费 GET /s/{token} → Consume：CAS 递增 / 一次性删除（statestore.md §3.2 有界重试）
副本节点（只读副本设计 §2.3）：普通 token 放行且跳过计数写；一次性 token → 503
```

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| Get 未命中 | 凭据→(nil,nil)；索引→回退 WalkDir；分享→404 | 按「无此状态」 |
| Get 值损坏 | 凭据→启动失败（fail-closed）；索引→回退重建+Warn | 凭据是权威，索引是加速层 |
| Put/CAS 失败 | 凭据/索引 fail-closed（写路径报错+日志）；分享/审计尽力而为 | 保留各 Store 现状语义 |
| mongo 不可达（运行期） | 写面 503+日志；读面 meta 503、纯文件下载不受影响（文件在共享卷） | 读降级见只读副本设计 |
| 装配期 mongo 探活失败 | 启动失败（statestore.md §2.4） | fail-fast |
| 集群模式但 StateStore 未装配 | 启动失败 | 防假一致 |

## 5. 测试 + 变异点

- `TestStateBackedCredentialStorer_Compat`：读旧 `<meta>/credentials.json` 格式可载入（格式兼容断言）；
- `TestCluster_MainWriteReplicaRead`：**两个 Handlers 注入同一 mock 共享 StateStore** → 主节点 register/upload → 副本节点签名校验/列表读到同一状态（核心验收）；
- `TestCluster_DedupShared`：两节点（同 StateStore）Add 同 checksum → 台账一致；变异：副本落本地 JSON → 红；
- `TestCluster_IndexSnapshotLoad`：主 saveAll → 副本 ensureOwner 载入快照（注入计数断言未 WalkDir）；变异：副本忽略快照恒 WalkDir → 红；
- `TestCluster_NoStateStore_ZeroRegression`：默认配置（local 单节点）既有测试全绿；变异：装配层把 nil state 误当已装配 → 红；
- 变异：凭据 Get 损坏**静默重建**（应 fail-closed）→ 红。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **S1** | 集群模式判定 + `StateBackedCredentialStorer`（含加密链保留）+ 凭据切 StateStore | 凭据兼容测试 + 主写副本读测试绿 | statestore F1/F2 |
| **S2** | `DedupStore` 可选 state 后端 + checksum 台账同款 | 既有 dedup_test 双跑全绿（断言不改） | S1 |
| **S3** | 分享适配器（CAS）+ 索引快照后端切换 | Consume 并发无丢失；索引快照载入测试绿 | S2 |
| **S4** | 副本读面降级语义（meta 503 / 文件下载不受影响） | 降级测试绿 | S1-S3 |
| **S5** | cluster 配置段 + docs/config.md（R15）+ 两节点 e2e | e2e 主写副本读绿；文档门禁绿 | S4 |

依赖图：S1 → S2 → S3 → S4 → S5（与 statestore F2/F3、leader F2 装配期合流）。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 配额跨节点双计残留 | LeaderElector 写面唯一（文档明示：StateStore 不解决该问题——statestore.md §8 同款） |
| 迁移后落盘路径变化回滚难 | 双读单写（读旧 meta 回退 + 首写迁新路径），每片独立可回滚 |
| 副本读到旧快照 | 文档明示：实时一致性由 Watch 失效广播（statestore.md §3.4）后续片承担 |
| 凭据 AD-5 不变式 | 仅集群模式切 StateStore；单节点 local 路径与文件格式不变 |
| 一次性分享计数超发（副本） | 只读副本设计 §2.3 决断：一次性 token 副本 503 |
| R21 门禁误伤 | 豁免清单逐 Store 收紧 + 自检 + 变异验证（statestore.md §6.3） |

**零回归保证**：① 默认 `type=local` + `cluster.enabled=false` → 全部原路径原文件；② 未装配 StateStore → 不注入任何适配器；③ 每 Store 既有测试套件不改断言全绿（适配器双跑）；④ 凭据 fail-closed / 分享尽力而为语义保留；⑤ 领域包零改动或仅加可选 nil 字段。
