# StateStore 状态抽象接口（插件化 mongo/raft）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：新包 `pkg/state` + 装配层迁移 + 门禁
> 关联设计：[2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（LeaderElector 与 StateStore 同包共存）

## 1. 背景与目标

### 1.1 现状痛点

sproxy 的「状态」分散在多个本地 JSON 落盘或纯内存实现中，各自为政：

| 状态 | 现状位置 | 存储形态 | 原子性 | CAS |
|---|---|---|---|---|
| 凭据 Ring | `pkg/accesskey/credentialstore.go` | `<meta>/credentials.json`（tmp+rename+fsync） | 全量快照 | 无（`AddRegistration` 内存 CAS） |
| checksum 台账 | `pkg/checksum/store.go` | `<meta>/checksums.json` 全量 map 快照 | tmp+rename | 无 |
| dedup 台账 | `pkg/files/dedup.go` | `<meta>/dedup.json` 全量 map 快照 | tmp+rename | 无（引用计数内存 RWMutex） |
| 分享链接 | `pkg/server/share.go` | `<share>/<token>.json` 逐条 | tmp+rename | 无（Downloads 计数内存 RWMutex） |
| 索引快照 | `pkg/files/index_persist.go` | `<meta>/index/<owner>.json` | tmp+rename | 无 |
| 审计 | `pkg/server/audit_store.go` | `audit.log` append-only JSON lines | O_APPEND | 无 |
| 配额 | `pkg/quota/quota.go` | 纯内存 Pool/Scope | 内存原子 | 内存 |
| 用户卷 meta | `pkg/server/user_volume_store.go` | `<meta>/volume/<name>.json` | tmp+rename | 无 |

共同问题：
1. **每份都有重复的原子写样板**（tmp+rename+Windows 回退+saveMu 串行化），`pkg/files/dedup.go`、`pkg/checksum/store.go`、`pkg/accesskey/credentialstore.go` 三处几乎逐字重复；
2. **多节点挂同一外部卷时无写面仲裁**：checksum/dedup/分享/配额各节点各写各的本地 JSON，静默互相覆盖；
3. **无统一 CAS 原语**：配额、dedup 引用计数、凭据并发登记在跨进程场景下只能靠「读-改-写」非原子序列；
4. **无 Watch**：索引失效广播、跨节点缓存失效需要轮询或人工触发。

### 1.2 目标

- 定义统一 `StateStore` 抽象：`Get/Put/Delete/List/CAS`，可选 `AppendStore`（审计/事件）与 `WatchStore`（索引失效广播）；
- 本地 JSON 为默认实现（**零回归**：装配语义、落盘路径、原子写行为与现状逐字一致）；
- Mongo 实现为集群方案 A 的配套（文档集合 `_id=key` + `findAndModify` CAS）；
- Raft 实现（`etcd/raft`）为长期目标，本期只留接口位与注册表分派（不实现）；
- 装配层仿 `pkg/volume/registry` 的 `RegisterBackend` 模式：`RegisterStateStore(name, factory)`；
- 门禁：禁非测试源码直接 `os.WriteFile(meta/*)` / 裸 JSON 落盘（引导新代码走 StateStore）。

### 1.3 非目标

- 不做跨实现数据迁移工具（local→mongo 由运维脚本/外部完成；本期只保证**读旧格式兼容**）；
- 不做 mongo 之外的第二种分布式后端；
- 不把文件内容（user 桶）纳入 StateStore——状态存储只管 meta 状态，文件本体仍走 storage.Root；
- Raft 本期不实现，仅注册表预留 `"raft"` 类型并 fail-closed（未实现即启动报错）。

## 2. 组件与接口

### 2.1 新包 `pkg/state` 结构

```
pkg/state/
  state.go          # 核心接口 StateStore / AppendStore / WatchStore / Change / ErrKeyNotFound
  registry.go       # RegisterStateStore / NewStateStore / StateStoreTypes / UnregisterStateStoreForTest
  local.go          # LocalStateStore（JSON 文件封装，目录分片 key → <root>/state/<owner>/<type>/<name>.json）
  local_cas.go      # LocalStateStore.CAS（文件锁 + read-modify-write）
  local_watch.go    # LocalStateStore.Watch（文件变更轮询 → Change chan）
  append_local.go   # LocalAppendStore（append-only JSON lines）
  key.go            # Key 规范化（owner/type/name 三段 + 路径安全段名校验，复用 storage.ValidSegmentName 语义）
  mongo.go          # MongoStateStore（集合 _id=key；findAndModify CAS；TTL 用于租约）
  mongo_append.go   # MongoAppendStore（审计/事件，按 key 前缀追加数组）
  config.go         # 配置模型 StateStoreConfig{Type, Dir, Mongo{URI,Database,Collection}}
  doc.go            # 包注释（约定与安全边界）
```

### 2.2 核心接口（任务给定形状，补充约定注释）

```go
// StateStore 是状态存储抽象：key → 不透明字节值。
// 实现约定（跨实现一致）：
//   - Get：key 不存在返回 ErrKeyNotFound（与 os.ErrNotExist 语义对齐），绝不返回 (nil, nil)；
//   - Put：原子写（tmp+rename 语义——本地实现；mongo 为 upsert 单文档），
//     成功返回后读必须看到新值（线性一致性，由 mongo 主节点或本地 rename 保证）；
//   - Delete：key 不存在静默成功（幂等）；
//   - List：返回 prefix 前缀下的完整 key 列表（排序不承诺稳定；调用方自行排序）；
//   - CAS：old 为 nil 表示「期望不存在」（create-only），new 为 nil 表示「期望删除」。
//     失败（当前值 ≠ old）返回 ErrCASMismatch。必须原子——禁止读-改-写非原子序列。
type StateStore interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Put(ctx context.Context, key string, data []byte) error
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) ([]string, error)
    CAS(ctx context.Context, key string, old, new []byte) error
}

// ErrKeyNotFound 是 Get 未命中的哨兵错误（各实现统一）。
var ErrKeyNotFound = errors.New("state: key not found")

// ErrCASMismatch 是 CAS 期望值不匹配的哨兵错误（调用方按 409/重试处理）。
var ErrCASMismatch = errors.New("state: CAS mismatch")

// AppendStore 是可追加存储（审计/事件）：Append 把 data 作为一条记录追加到 key 的尾部。
// 语义：线性追加、不覆盖历史；Read/List 由调用方按需读取（本接口不定义读取）。
type AppendStore interface {
    Append(ctx context.Context, key string, data []byte) error
}

// Change 是 Watch 发出的变更事件。
type Change struct {
    Key   string // 变更的完整 key
    Op    string // "put" | "delete"
    // Prev 是变更前的值（仅 put 且实现支持时填充；nil = 实现不提供）。
    Prev  []byte
}

// WatchStore 是可观察存储：Watch 订阅 prefix 下的变更流。
// 实现约定：
//   - 初始不重放现有键（只推后续变更）——索引失效场景需要的是「后续写」；
//   - 通道由调用方负责消费；底层存储不可用时通道关闭（调用方应退避重连）；
//   - ctx 取消时关闭通道并返回。
type WatchStore interface {
    Watch(ctx context.Context, prefix string) (<-chan Change, error)
}
```

### 2.3 key 规范与目录分片（LocalStateStore）

key 采用三段式：`<owner>/<type>/<name>`，如：

```
credential/anonymous/ring
checksum/<owner>/<rel>          # rel 中 "/" 保留（checksum 台账 key 本身就是路径）
dedup/<owner>/<checksum>
share/<token>
index/<owner>
audit/<seq>
quota/<owner>
leader/global                    # 与 LeaderElector 共享（见 2026-09-24-leader-elector.md）
```

- 每段经 `validateSegment`（仿 `pathguard.ValidateFilePath` / `storage.ValidSegmentName`）：拒绝 `..`、绝对路径、空字节、Windows 非法字符；`/` 仅作为段间分隔符，`name` 段内部允许 `/`（checksum rel）；
- 落盘路径：`<root>/state/<owner>/<type>/<name>.json`（`name` 段内部 `/` 转目录层级）；
- **零回归保证**：装配层把既有 JSON 文件**原样保留**为 StateStore 之上的「首版导入」——`NewLocalStateStore` 扫描 `<root>/state/`，而既有 `meta/` 路径由各 Store 适配器在**读路径**回退（见 §5 迁移策略），不要求一步搬文件。

### 2.4 注册表（仿 `pkg/volume/registry/backend.go`）

```go
// StateStoreFactory 按配置构造状态存储（配置校验失败返回错误，装配期 fail-fast）。
type StateStoreFactory func(cfg StateStoreConfig, logger *slog.Logger) (StateStore, error)

func RegisterStateStore(typ string, f StateStoreFactory)  // 重复注册 panic；空名 panic（同 registry.RegisterBackend）
func NewStateStore(typ string, cfg StateStoreConfig, logger *slog.Logger) (StateStore, error) // 未注册 fail-closed 报错
func StateStoreTypes() []string                            // 已注册类型列表（副本）
func UnregisterStateStoreForTest(typ string)               // 测试辅助；生产代码不得调用
```

- **不允许静默回落 local**：`state_store.type: mongo` 但 mongo 未注册/连不上 → 启动失败（fail-closed），不回落本地（回落会造成「以为多节点一致、实际各写各的」的最坏情况）；
- 默认注册：`local`（内置）；`mongo` 由 `pkg/state` 内置（依赖 `go.mongodb.org/mongo-driver`，遵循依赖策略需评审——Mongo 官方纯 Go 驱动，符合「纯 Go、API 稳定、社区活跃」标准，建议放行）；`raft` 注册表预留但**不注册**（配置 `raft` → 启动报「未实现」）。

### 2.5 MongoStateStore

- **依赖隔离（已决策）**：Mongo 实现放独立 module（`ext/state/mongo`，仿 `pkg/tunnel/xfer/ext/*` 模式），仅 `cmd/sproxy` 装配层引入 `go.mongodb.org/mongo-driver`，领域包（`pkg/state` 核心）不引——保持核心零外部依赖。
- 集合：配置 `state_store.mongo.collection`（默认 `sproxy_state`）；文档 `_id = key`，字段 `{v: <bytes>, rev: <int64>}`；
- `Put`：`ReplaceOne`（upsert，`rev` 自增）；
- `CAS(old, new)`：`findOneAndUpdate` 带 filter（`_id=key` 且 `rev=sha256(old)` 或 `v=old`）→ 语义等价于「读当前 → 比对 → 原子替换」。**实现细节**：为可靠比对用 `rev` 版本号字段 + 读-比-改在 mongo 事务内（或单文档 findAndModify 内嵌比对 JSON），两者取其一，测试以并发 CAS 正确性为验收；
- `Delete`：`DeleteOne`（未命中静默）；
- `List(prefix)`：`Find({_id: regex ^prefix})`（或 `_id >= prefix && _id < prefix\uffff` 范围查询）；
- `Append`（MongoAppendStore）：文档 `{seq: <inc>, events: [b1,b2,...]}`，`$push` 到 `events` 数组（上限 `maxAppendBatch`，超限滚动到新 seq 文档）——审计查询按 seq 范围读；
- `Watch`：Mongo **Change Streams**（`watch()` + `$match: {documentKey._id: /^prefix/}`），Fallback 到 `_id` 范围轮询（Change Streams 不可用/不支持时降级，日志告警——可观测铁律）；
- 连接管理：驱动内置连接池；`Ping` 探活装配期校验（仿 Vault 启动探活模式）。

### 2.6 LocalStateStore 实现要点

```go
type LocalStateStore struct {
    root   string          // <root>/state
    mu     sync.Mutex      // 串行化跨 key 的 saveMu 域（Windows rename 并发防护同 saveMu 语义）
    logger *slog.Logger
}

func (s *LocalStateStore) Put(ctx, key, data) error {
    // MkdirAll(dir) → tmp 同目录 → WriteFile(tmp) → Rename(tmp, target)
    // Rename 失败回退直接写（对齐既有 store 的 Windows 回退语义）+ Warn 日志
}

func (s *LocalStateStore) CAS(ctx, key, old, new) error {
    // 持 key 级锁（per-key mutex map，仿 UserVolumeStore.ownerLocks）
    // 读当前（不存在 → 空）→ bytes.Equal(old, cur) 不匹配 → ErrCASMismatch
    // new == nil → Delete；否则 Put（复用原子写）
}

func (s *LocalStateStore) Watch(ctx, prefix) (<-chan Change, error) {
    // 后台轮询（默认 500ms 间隔 + 抖动）：List(prefix) diff 上次快照
    // 推 put/delete；ctx 取消退出。轮询间隔可注入（测试用短间隔）。
}
```

## 3. 数据流

### 3.1 上传去重 + 引用计数（写路径，重点 CAS 场景）

```
POST /upload
  → files.Upload → DedupPolicy.DedupStoreFor(owner) → DedupStore(适配器)
  → adapter.Add(rel, vol, checksum):
      CAS(key=dedup/<owner>/<checksum>, old=null, new=json{refs:[rel]})  // 首份
      失败(ErrCASMismatch) → Get → 追加 ref → CAS(old=当前, new=追加后)   // 重试循环
  → 配额 adapter.Reserve：CAS(quota/<owner>, old=当前账本, new=当前+size)
```

跨节点一致性由 CAS 保证：两个节点同时上传同内容/同 owner 时，只有一个成功获得首份语义。

### 3.2 分享链接 Downloads 计数（读-改-写收敛为 CAS）

```
GET /s/{token}
  → ShareStore.Consume(token) → adapter：
      for {
        cur = Get(share/<token>)           // 不存在 → 已消费返回 nil
        new = bumpDownloads(cur)           // Downloads++ / 一次性删除 → CAS(old=cur, new=null)
        if CAS 成功 break
      }
```

### 3.3 审计（Append 流）

```
RecordAudit → AuditStore.Append(evt)
  → LocalAppendStore.Append("audit/<seq>", jsonLine)   // O_APPEND 单次 Write
  → MongoAppendStore.Append("audit/<seq>", jsonLine)   // $push events 数组
```

审计是尽力而为（现有语义：失败记日志不阻断业务），AppendStore 保持该语义。

### 3.4 索引失效广播（Watch 流）

```
节点 B（主）写路径更新索引 → Put(index/<owner>, snapshot)
节点 A 的 searchIndex → Watch("index/") → 收到 put index/<owner> → InvalidateIndex(owner)
```

本期 Local Watch 的轮询 diff 在**同进程多实例**场景即已解决「旁路写后的索引失效」（如版本恢复 InvalidateIndex 的手动调用可保留，Watch 只作补充）。

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| `Get` 未命中 | 返回 `ErrKeyNotFound` | 调用方按「无此状态」处理（如 share token 无效 → 404） |
| `CAS` 期望值不匹配 | 返回 `ErrCASMismatch` | 调用方重试循环（有界，如 8 次）或按冲突拒绝（409） |
| 本地写失败（磁盘满/权限） | 返回 error | checksum/dedup 沿用现状：记日志 + 重试一次；凭据沿用 fail-closed |
| mongo 不可达 | 返回 error | 写路径按各自语义（凭据 fail-closed；分享/审计尽力而为）；启动装配时 Ping 探活失败 → 启动失败 |
| `Watch` 底层不可用 | 通道关闭 + 日志 | 调用方退避重连（指数退避上限封顶） |
| key 非法（含 `..`/绝对路径） | 返回 error | 所有实现统一在 key.go 校验（fail-closed，绝不静默改写） |
| `state_store.type` 未注册（raft） | 装配期报错 | 不回落 local（防多节点假一致） |

## 5. 对既有代码的影响（哪些 Store 要迁移）

### 5.1 迁移矩阵（F2 逐 Store 适配）

| Store | 现文件 | 适配方式 | 迁移优先级 | CAS 必要？ |
|---|---|---|---|---|
| `accesskey.CredentialStore` | `pkg/accesskey/credentialstore.go` | **保持 CredentialStorer 接口不动**，新增 `StateBackedCredentialStore{st: StateStore, key: credential/anonymous/ring}`；`Load/Save` 委托 Get/Put | P0（凭据是权威） | 是（并发登记） |
| `checksum.ChecksumStore` | `pkg/checksum/store.go` | 保留现有实现作 local 后端；新增 StateStore 适配器把 `map[string]string` 整体序列化为一个 key | P0 | 否（全量快照足够） |
| `files.DedupStore` | `pkg/files/dedup.go` | 同上（台账整体序列化） | P0 | 是（引用计数跨节点） |
| `server.ShareStore` | `pkg/server/share.go` | 逐 token key：`share/<token>`；Consume 计数走 CAS | P1 | 是 |
| `files` 索引快照 | `pkg/files/index_persist.go` | `index/<owner>` 单 key | P1 | 否（快照覆盖） |
| `server.AuditStore` | `pkg/server/audit_store.go` | AppendStore 适配（`audit/<seq>`） | P1 | N/A |
| `quota.Pool/Scope` | `pkg/quota/quota.go` | **不迁移到 StateStore**——配额是高频内存账本，持久化由「周期快照 + 重启 reconcile」承担；仅当启用 `quota.persist: true` 时才落 `quota/<owner>` 快照 key | P2 | 是（reconcile 防双计） |
| `server.UserVolumeStore` | `pkg/server/user_volume_store.go` | `volume/<owner>/<name>` | P2 | 否 |

**零回归铁律**：迁移后**默认路径与文件格式不变**。LocalStateStore 把 `<root>/state/<owner>/<type>/<name>.json` 作为**新默认落盘路径**，但 F2 每片交付时保留「读旧 `meta/` 路径回退 + 首次写迁新路径」的双读单写策略（与 dedup `meta/dedup.json` 同目录语义，避免一步搬文件导致回滚困难）。

### 5.2 装配层改动

- `cmd/sproxy/root.go`：`state_store: {type: local|mongo, dir: <root>/state}` 配置段解析 + `NewStateStore` 装配，把 `StateStore` 注入 `RegisterRoutesOpts`；
- `pkg/server`：`Handlers` 新增 `stateStore StateStore` 字段（nil = 未装配零回归）；`RegisterRoutes` 按配置把 StateStore 派发给各 Store 适配器；
- `pkg/files/options.go`：能力接口**不新增**——DedupPolicy/ChecksumLedgers 保持现状，适配发生在装配层（适配器实现既有接口、内部委托 StateStore）。避免领域包依赖 pkg/state 的方向性问题（门禁 R2 同款：领域包只 import 顶层包；`pkg/state` 是 G0 基础包，领域包可 import，但**首选**保持接口不动、装配层做适配）。

### 5.3 配置模型

```yaml
state_store:
  type: local          # local | mongo | raft（raft 未实现，装配报错）
  dir: "./storage/state"  # local 专属；空 = <storage_root>/state
  mongo:
    uri: "mongodb://127.0.0.1:27017"
    database: "sproxy"
    collection: "sproxy_state"
```

`SetDefaults`：`type` 空 → `local`（零回归）。`Validate`：`mongo` 必填 uri；`raft` → 响亮拒绝（未实现）。

## 6. 测试 + 变异点

### 6.1 单元测试（`pkg/state`）

- `TestLocalStateStore_RoundTrip`：Put/Get/Delete/List 全流程 + key 分段落盘路径断言；
- `TestLocalStateStore_AtomicWrite`：并发 Put 同 key（20 goroutine）→ 文件恒完整（无 torn write），变异点：去掉 tmp+rename 改直写 → 红；
- `TestLocalStateStore_CAS`：成功路径 / old 不匹配 → `ErrCASMismatch` / create-only（old=nil 且已存在 → 失败）/ delete 语义（new=nil）；
- `TestLocalStateStore_CAS_Concurrent`：8 goroutine 对同一 key 做「读-改-写」CAS 递增 → 最终计数 = 8×N（无丢失更新）；变异点：CAS 实现退化为「先读后写非原子」→ 红；
- `TestLocalStateStore_Watch`：Put/Delete 后收到对应 Change；ctx 取消关闭通道；轮询间隔注入 10ms（短等待条件轮询 `testutil.WaitFor`，不引 time.Sleep——R14 棘轮）；
- `TestRegistry_RegisterStateStore`：重复注册 panic / 空名 panic / 未注册 NewStateStore 报错（fail-closed）/ `StateStoreTypes` 副本；
- `TestKeyValidate`：`..` / 绝对路径 / 空字节 / Windows 非法字符拒绝；`checksum/<owner>/dir/f.txt`（name 段含 `/`）放行；
- `TestMongoStateStore`（`//go:build mongo_integration` 或依赖内存 mongo——**不引第三方内存 mongo**，仅在有真实 mongo 的 CI job 跑，本地跳过）：CRUD + CAS 并发 + List 前缀 + Append + Watch（Change Streams 若不可用验证 fallback 轮询告警）。

### 6.2 迁移回归（各 Store 适配器）

- `TestStateBackedCredentialStore_Compat`：读旧 `<meta>/credentials.json` 格式 → 适配器可载入（文件格式兼容断言）；
- `TestDedupStore_StateAdapter`：既有 `dedup_test.go` 全量跑在新适配器上（同一行为套件，双实现跑）——**关键验收**：把 DedupStore 的落盘从自管 JSON 切到 StateStore 后 `dedup_test.go` 不改断言全绿；
- `TestShareStore_StateCAS`：Consume 并发（同 token 10 goroutine 同时消费）→ 计数无丢失（OneTime 只成功一次）。

### 6.3 门禁 + 变异验证

- **R21 `meta_file_gate_test.go`**（仿 R19 http_transport_gate 模式，`internal/archcheck/`）：
  - 扫描非测试源码：禁止 `os.WriteFile(` + 含 `meta` 或 `state` 路径段；禁止 `os.CreateTemp(` 于 `meta`；
  - 豁免：`pkg/state` 自身实现、门禁自身、既有迁移期 Store（**F2 完成后收紧豁免**——先从豁免清单删掉已迁移的 Store，最终只剩 pkg/state）；
  - 变异验证：把豁免清单中某已迁移文件加回 → 红；把 `pkg/state` 自身误伤 → 红（自检 TestScanMetaFile_ScopeAndWordBoundary 仿 R11 自检）；
- **R22 `state_cas_gate_test.go`**（可选，若配额/分享未来跨进程）：断言生产代码中「Get 后 Put 同 key」模式出现处必须经过 CAS 包装（源码级模式匹配，防御回归）。
- 测试规范：全部顶层 Test `t.Parallel()`；文件系统用例用 `t.TempDir()`；127.0.0.1 绑定仅 mongo 集成用；无 `time.Sleep`（R14）。

## 7. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **F1** | `pkg/state` 接口 + `LocalStateStore` + `LocalAppendStore` + 注册表 + key 校验 + R21 门禁（含豁免清单初版） | 单测全绿 + 变异命中；门禁自检通过 | 无 |
| **F2** | 各 Store 适配迁移（§5.1 矩阵，按 P0→P2 顺序，每 Store 一片或合片）：CredentialStore → ChecksumStore/DedupStore → ShareStore → AuditStore → 索引快照 | 每个 Store 既有测试套件在新适配器上全绿（断言不改）；读旧格式兼容测试绿 | F1 |
| **F3** | `MongoStateStore` + `MongoAppendStore` + 装配探活 + config（mongo 集成测试） | mongo 集成测试绿（本地跳过）；探活失败启动报错 | F1 |
| **F4** | LeaderElector（同包，见另一份设计） | 见 leader-elector 设计 | F1 |
| **F5** | RaftStateStore（etcd/raft）：仅接口实现骨架 + 注册表挂 `raft` 类型 + 未实现 fail-closed 文档化 | 设计评审通过即视为完成（本期不交付实现） | F3 |

依赖图：F1 → F2 → F3；F4 ∥ F2/F3（同包不同文件）；F5 最后。

## 8. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 迁移后落盘路径变化导致回滚困难 | F2 双读单写策略：读旧 `meta/` 回退 + 首写迁新路径；每片独立可回滚 |
| Mongo CAS 语义偏差（rev vs 值比对） | rev 字段 + 单文档原子更新；并发测试作为验收门 |
| Watch 轮询开销（本地实现） | 默认间隔 500ms + 仅 diff；同进程写路径仍走显式 InvalidateIndex（Watch 只是补充广播） |
| mongo 驱动引入新依赖 | 官方纯 Go 驱动（符合依赖策略）；F3 评审时确认版本与 license |
| 门禁误伤（R21） | 豁免清单逐 Store 收紧 + 自检用例 + 变异验证 |
| 配额不迁移导致的「多节点双计」残留 | 文档明示：配额跨进程一致性依赖 LeaderElector（写面唯一），StateStore 不解决该问题 |
| `pkg/state` 被领域包反向依赖形成环 | 保持「领域包接口不动、装配层适配」策略；archcheck 现有依赖方向门禁覆盖 |

**零回归保证清单**：
1. 默认 `state_store.type` 空 → local，路径 `<storage_root>/state/` 下新落盘 + 读旧 meta 回退；
2. 未配置 state_store → `Handlers.stateStore == nil`，全部现有 Store 走原实现（零行为变化）；
3. 每 Store 迁移后其既有测试套件原样通过（适配器双跑）；
4. 审计/分享的「尽力而为」语义（失败不阻断业务）在新实现中保留；
5. 凭据 fail-closed（载入失败拒绝启动）在新实现中保留。

## 9. 关联项

- LeaderElector：`leader/global` key 与本地 `flock` 落在同一 `pkg/state` 包（见 [2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)）；
- 多节点写面唯一化是 StateStore 跨节点一致的**前置条件**：CAS 解决「并发冲突检测」，LeaderElector 解决「谁有权写」；
- config.md / docs/cli.md 的 `state_store` 段文档（R15 文档漂移门禁要求装配期同步更新 docs/config.md）。
