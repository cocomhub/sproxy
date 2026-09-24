# 集群只读副本接入（11.11-③）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：副本装配 + 读面路由清单 + 降级语义
> 关联：[2026-09-24-leader-elector.md](./2026-09-24-leader-elector.md)（WriteGuard 已设计——本任务补「只读副本的读面装配」细节）、
> [2026-09-24-cluster-state-migration.md](./2026-09-24-cluster-state-migration.md)（状态上移：副本读共享 StateStore）
> **本任务是 leader-elector.md §2.5 的读面补充**：哪些路由不设门、federated 只读形态如何复用、写降级语义。

## 1. 背景与目标

### 1.1 现状（源码实证）

- `pkg/volume/federated/federated.go`：只读适配层已存在——`New(reader)` 构造、**未注入 Writer 时写方法恒 `ErrReadOnly`（fail-closed）**；`WithWriter(w)` 可选注入写面；
- 文件读面（`GET /download`、`GET /api/files` 等）无写面依赖——天然可服务共享卷；
- WriteGuard（leader-elector.md §2.5）已设计：主放行、从节点 503。

### 1.2 目标

- 非主节点以**只读副本**接入：共享外部卷（只读挂载）+ WriteGuard（非主 → 503）+ 读面全开；
- **复用 federated.FS 只读形态**：副本的存储后端 = federated.FS（或共享卷直接挂载 + meta 读面走 StateStore），写方法恒 ErrReadOnly 兜底——即使 WriteGuard 漏网，存储层仍 fail-closed；
- 明确读面路由清单（哪些路由不设门）与写面清单（哪些必须设门）。

### 1.3 非目标

- 不做副本写转发（集群后续片）；
- 不做读一致性/实时同步（Watch 广播后续片，statestore.md §3.4）；
- 不改变单节点行为（cluster.enabled=false 零改动）。

## 2. 组件与接口

### 2.1 副本装配（`cmd/sproxy/root.go` 按 `cluster.mode: replica` 分叉）

```
副本节点装配：
  storage 后端 = 共享外部卷（只读挂载）→ 经 federated.New(reader) 或直接只读 Root
  meta 读面   = 共享 StateStore（Get/List 只读；见状态上移设计 §3）
  WriteGuard   = LeaderElector follower → isLeader=false → 写 503
  文件写面     = federated.FS 未注入 Writer → 恒 ErrReadOnly（双层兜底：路由门 + 存储门）
```

### 2.2 读写路由清单（WriteGuard 装配的精确边界——leader-elector.md §2.5 的落地版）

**写面（必设门，`WriteGuard.Authorize()` 前置）**：

| 路由 | 说明 |
|---|---|
| `POST /upload`、`/upload/init|chunk|complete` | 分块族全设 |
| `POST /delete`、`/rename`、`/mkdir`、`/rmdir` | 文件/目录写 |
| `POST /api/share` | 创建/撤销分享（写 meta） |
| `POST /api/versions/restore` | 版本恢复（旁路写 → 还须 InvalidateIndex） |
| `PUT /api/config` | 运行时配置 |
| `POST /api/credentials/*`（register/renew/rotate） | 凭据写 |
| `POST /api/cloud/download*` | 任务创建 |

**读面（不设门，副本照常服务）**：

| 路由 | 说明 |
|---|---|
| `GET /download`、`/download/chunk`、`/s/{token}`（普通 token） | 文件/分享读 |
| `GET /api/files`、`/api/files/stat`、`/api/files/search`、`/api/versions`（查询） | 列表/元信息/搜索 |
| `GET /api/cloud/tasks`、`/healthz`、`/version`、`/metrics`、`/ui/` | 只读/诊断 |

**判定口诀**：路由语义「改状态（文件/meta/配置）」→ 设门；「只读状态（文件/元信息）」→ 不设门。

### 2.3 特殊读路径的副本语义

- `GET /s/{token}`：普通 token 放行（Downloads 计数写**跳过**——副本不写 share meta，计数仅主节点累计）；**一次性（one-time）token 副本一律 503**（读即消费语义依赖计数写，副本无法原子消费——防同一 token 多副本重复读导致超发）；
- `POST /api/archive`（创建压缩/解压任务）：任务落 meta 属写面 → 设门；已建任务的产物下载走读面；
- 版本恢复/`PUT /api/config` 之外的所有旁路写：副本一律拒绝，唯一例外是**审计 append**（尽力而为，副本可本地记审计不写共享 StateStore——statestore.md §4 语义保留）。

### 2.4 federated 复用与差异

federated.FS 原语义是「远端 hub 卷只读挂载」（网络 Reader 注入）。只读副本**复用其只读形态**但注入**本地共享卷读实现**（本地 `Root` 包装实现 `federated.Reader`：ListDir/Stat/OpenRead 委托本地只读 Root）——不引入网络依赖，只借用「未注入 Writer 恒 ErrReadOnly」的 fail-closed 门。写路径返回的 `ErrReadOnly` 在 HTTP 层统一映射 503（与 WriteGuard 同响应码，客户端无需区分原因）。

## 3. 数据流

```
客户端 GET /download → 读面路由（无门）→ 共享卷只读 Root → 200
客户端 POST /upload → WriteGuard.Authorize() → isLeader=false → 503 {"success":false,
  "message":"当前节点非主节点"} + Retry-After: 1
  （若门被绕过 → storage 后端 federated.FS.WriteFile → ErrReadOnly → 仍 503，双层兜底）
客户端 POST /api/share → 503；GET /s/{token}（普通）→ 200 读共享卷；GET /s/{token}（一次性）→ 503
```

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| 副本节点写请求 | 503 + Retry-After: 1 | 客户端重试/重定向主节点 |
| WriteGuard 漏网 → 存储层 | `ErrReadOnly` → 503 | 双层兜底（路由门 + 存储门） |
| 共享 StateStore 不可达（副本读） | meta 类读 503；文件下载 200（文件在共享卷，不依赖 meta 存储） | 读降级最小化 |
| 一次性 token 打在副本 | 503 | 防计数超发 |
| 审计写失败 | 本地记日志 | 尽力而为（现状语义） |

## 5. 测试 + 变异点

- `TestReplica_Read200_Write503`：副本装配（isLeader=false）→ `GET /download` 200、`POST /upload` 503 且**业务逻辑未执行**（fake uploadStore 断言未调用——防门后漏写）；变异：从节点仍执行上传逻辑 → 红；
- `TestReplica_StorageDoubleGuard`：WriteGuard 放行（模拟漏网）但存储层 federated.FS 未注入 Writer → 仍 503（`ErrReadOnly` 映射）；变异：去掉存储层兜底只留路由门 → 红；
- `TestReplica_ReadRoutes_NoGate`：读面清单逐路由断言**不**经 Authorize（装配断言：读 handler 无 WriteGuard 依赖）；变异：读面误设门 → 红（防过度设门破坏副本读）；
- `TestReplica_OneTimeToken503`：一次性 token → 503；普通 token → 200 且不写计数（计数写被跳过断言）；变异：副本也做消费写 → 红；
- `TestReplica_ErrReadOnly_HTTPMapping`：`ErrReadOnly` → 503 响应断言；
- `TestFederated_FS_ReadOnly`（既有）：`New(r)` 未 WithWriter → 写方法恒 ErrReadOnly（回归保住）；
- 全量既有读面 handler 测试在副本装配下原样通过（零回归）。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **R1** | 副本装配分叉（cluster.mode=replica）+ 读写路由清单落代码（WriteGuard 按清单设门/不设门） | TestReplica_Read200_Write503 绿 | leader F2（WriteGuard）、S1 |
| **R2** | 存储双层兜底：共享卷只读 Root 实现 federated.Reader + ErrReadOnly → 503 映射 | TestReplica_StorageDoubleGuard 绿 | R1 |
| **R3** | 分享特殊语义：普通 token 跳过计数写 / 一次性 503 | TestReplica_OneTimeToken503 绿 | R2、S3（分享适配器） |
| **R4** | 副本读降级（meta 503 / 文件下载 200）+ 审计本地尽力而为 | 降级测试绿 | R3 |
| **R5** | 文档（docs/config.md cluster 段，R15）+ 两节点 e2e（主写 → 副本读 200 / 副本写 503） | e2e 绿；文档门禁绿 | R4、S5 |

依赖图：R1 → R2 → R3 → R4 → R5（与状态上移 S 系列并行合流）。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 门后漏写（新写端点漏设门） | 存储层 ErrReadOnly 双层兜底；读写路由清单评审 + 读面「不设门」装配断言测试 |
| 过度设门破坏副本读 | 读面清单逐路由「无 WriteGuard 依赖」断言（防回归） |
| 一次性分享计数超发 | 副本 503 决断（文档明示：计数写只在主节点） |
| 副本读到旧快照 | 文档明示：实时一致性由 Watch 失效广播后续片承担 |
| federated.Reader 语义被本地实现破坏 | 复用现有 federated 测试套件；新本地实现单独单测 |
| 审计重复/缺失（多副本本地记） | 文档明示：审计聚合是后续片；副本审计仅本地尽力而为 |

**零回归保证**：① `cluster.enabled=false`（单节点）→ 无任何副本装配分叉，WriteGuard 恒放行（leader-elector.md §7 同款）；② 读面路由行为与单节点逐字一致；③ 既有 federated 只读测试与全部读面 handler 测试不改断言全绿；④ 写面 503 仅出现于显式集群部署（预期新语义，不影响单节点客户端）；⑤ 双层兜底保证任何漏网写最终失败而不是静默落到本地（**禁静默降级**——最坏情况是「写被拒」，不是「写错地方」）。
