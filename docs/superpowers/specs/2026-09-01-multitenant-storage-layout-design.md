# 多租户存储布局重构设计（阶段6 工作项 C 深度重构）

- 日期：2026-09-01
- 分支：feature/mesh-p6-owner
- 状态：已批准（头脑风暴完成，待实现计划）

## 1. 背景与动机

PR #146 工作项 C 已破坏性引入租户维度目录：认证用户文件存 `uploadsDir/<owner>/`，云归档存 `.__cloud_archives__/<owner>/`，云任务文件存 `.__cloud__/<taskID>/`，分块会话存 `.__chunked__/<owner>/<id>/`，版本存 `<owner>/.__versions__/`，任务状态存 `.__downloads__/` 与 `.__sync__/`。

独立审计（2026-08-31）确认当前实现功能正确、测试覆盖良好，但存在结构性缺陷，若继续演进将反复返工：

1. **魔法前缀 + 按名扫描判定内部目录**：`.__` 前缀 + `isInternalDirPathPrefix` / `isInternalFirstName` / `hasInternalDirAtAnyDepth` 多套谓词散落，已出现读取侧守卫缺口（list subdir / archive-dir 可触达内部目录，P1）。
2. **云任务/云状态/同步状态在全局根、无 owner 维度**：租户统计 cloud=0，租户间共享全局目录树，单租户脏数据/异常影响全局扫描。
3. **跨平台反复踩坑**：Windows 反斜杠 vs 协议正斜杠（F2/M1 修 3 轮）、保留字/尾点/大小写/分隔符边界未覆盖。
4. **配额/统计全局化**：`StorageManager` 单实例全局账本，无法按租户独立，某租户异常挤占他人。
5. **目录判定与路径安全分散**：`joinSafePath` 手工 EvalSymlinks + 短路径名 + 前缀检查约 60 行，脆弱且难以覆盖全部平台边界。
6. **checksum key 命名空间碰撞**：版本用 `__version__`（单下划线）与用户可创建的 `__version__/...` 路径 key 碰撞；云归档与同名用户文件 key 碰撞。

## 2. 设计目标与原则

1. **废弃 `.__xx__` 魔法内部目录**：租户根下按功能物理隔离目录（`user/`、`cloud/`、`archive/`、`chunk/`、`version/`、`meta/`），用户命名空间与功能命名空间**物理分离**，彻底消灭"按名称特殊处理"。
2. **租户完全自包含**：数据 + 状态 + 缓存都在 `<tenant>/` 一个子根内；删除/备份/迁移租户 = 操作一个目录。
3. **严格隔离**：无全局根用户数据；未认证映射到 `anonymous/` 默认租户；非法 owner 请求 400 拒绝（fail-closed），绝不回落全局根。
4. **租户维度配额**：每租户独立可配置上限 + 全局兜底；配额领域通用化（Pool/Scope），不引入租户概念。
5. **路径安全由标准库保证**：全面切换到 `os.Root`（Go 1.26 标准库，防 `..` 逃逸/符号链接逃逸），退役手工 `joinSafePath`。
6. **记录存储可插件化**：抽象 `pkg/store`（字节 KV + 泛型 JSON 封装 + 插件注册表），meta 记录、mesh 数据统一接入，未来可扩展 MySQL 等后端。
7. **一步到位、破坏性升级**：分支未合入、无外部存量部署，不做数据迁移；预留布局版本钩子（`LAYOUT_VERSION`）。
8. **新功能先行、测试全绿后迁移旧实现**：并行双轨，逐阶段切换，现有 owner 测试全家桶作为功能一致性基线。

## 3. 新存储布局

```
<storage_root>/                          # 原 uploads_dir；配置键更名 storage_root
  LAYOUT_VERSION                         # 布局版本标记（如 "2"），未来迁移钩子
  <tenant>/                              # 租户根：直接使用 owner 名（段名校验 fail-closed）
    user/                                # 用户文件根——remotePath 相对此，list/search/download 根
    cloud/                               # 云任务文件：<taskID>/<file>
    archive/                             # 云归档：<name>.tar.gz
    chunk/                               # 分块会话：<sessionID>/<chunkNNNNN>
    version/                             # 版本快照：<userPath>/<versionID>
    meta/                                # 租户自包含元数据（经 pkg/store）
      checksums.json                     # checksum 缓存（key = 相对租户根全功能 rel）
      cloud/<taskID>.json                # 云任务状态
      cloud/groups/<gid>.json            # 云任务组状态
      sync/<taskID>.json                 # 同步任务状态
  anonymous/                             # 未认证默认租户（结构与上面完全同构）
```

关键性质：

- **命名空间物理隔离**：用户路径只可能出现在 `user/` 下，功能目录在 `user/` 之外 → 用户永远无法创建与功能目录重名的路径。
- **功能判定单入口**：一个 `Tenant` 方法回答"路径属于哪个功能桶/是否合法"，替代多套散落谓词。
- **租户自包含**：操作租户 = 操作 `<tenant>/` 一个目录。

## 4. 包结构终版

```
pkg/
  store/                 # 通用记录存储：Store 接口 + JSONStore[T] 泛型 + 插件注册表（叶子包）
  store/file/            # 默认文件实现（原子写 + 前缀遍历）
  storage/               # 文件布局：Root(os.Root) + Tenant + UserRel/FeatureRel + 段名校验
  quota/                 # 通用配额：Pool/Scope（路径化叠加）+ Reservation
pkg/server/              # handler 层，依赖上述三包；owner_path.go 分散逻辑收敛到 pkg/storage
pkg/server/syncmgr/      # 依赖 storage + store + quota（修复 validOwnerName 漂移）
```

依赖方向：`pkg/store` 是叶子（不依赖任何业务包）；`pkg/storage` 依赖 `pkg/store`（meta 桶）；`pkg/quota` 独立无依赖；`pkg/server` 与 `pkg/server/syncmgr` 依赖三者。无环。

### 4.1 pkg/store：通用记录存储

```go
// 字节级 KV，插件友好（MySQL 等只需实现字节 KV）
type Store interface {
    Get(key string) ([]byte, error)
    Set(key string, value []byte) error    // 原子写（tmp + rename + 锁）
    Delete(key string) error
    List(prefix string) ([][]byte, error)  // 前缀遍历
    Close() error
}

// 泛型封装：类型安全 + 原子 JSON 编解码
type JSONStore[T any] struct{ s Store }
func (j *JSONStore[T]) Get(key string) (*T, error)
func (j *JSONStore[T]) Set(key string, v *T) error
func (j *JSONStore[T]) List(prefix string) ([]*T, error)

// 插件注册表（复用 pkg/plugin 模式；本次只实现 file，预留 mysql 等）
Register(name string, factory func(cfg StoreConfig) (Store, error))
```

- 默认实现 `store/file`：JSON 文件 + 原子写，合并现有 `.checksums.json`、`.__downloads__/<id>.json`、`.__sync__/<id>.json`、hub persist 四套重复原子写逻辑为单一实现；前缀 `List` = 目录遍历。
- 使用方：租户 meta（checksum / 云任务 / 云任务组 / 同步任务）；hub 持久化（mesh 路由表 / 信令收件箱 / 联邦节点表快照，`JSONStore[hub.Snapshot]`）；未来云任务组、分享持久化、MySQL/Redis 插件。

### 4.2 pkg/storage：文件布局

```go
// Root 封装 os.Root + LAYOUT_VERSION 校验
func OpenRoot(path string) (*Root, error)
func (r *Root) Open(rel string) (*os.File, error)
func (r *Root) Stat(rel) / MkdirAll(rel) / Remove(rel) / Rename(a, b) / OpenFile(rel, ...)

// Tenant：租户值类型，持有自己的 *Root 与配额 Scope
type Tenant struct { ID string; root *Root; quota *quota.Scope }
func (t *Tenant) UserRel(remotePath string) (rel string, ok bool)     // 用户输入 → user/ 桶下安全 rel
func (t *Tenant) FeatureRel(bucket, sub string) (rel string, ok bool)  // 服务端内部功能路径
func (t *Tenant) MetaStore() *store.JSONStore[...]                     // meta 桶接入 pkg/store
```

- 所有 handler 只用 `UserRel` / `FeatureRel` 构造路径，磁盘操作经 `Root`（os.Root）→ 防穿越/符号链接由标准库保证。
- `os.Root` 未覆盖的操作（如 `os.Chtimes`、`os.SameFile` TOCTOU 交叉校验）统一由 `Root` 提供薄封装：以 root 内已校验的 rel 派生路径执行，并保持与 os.Root 相同的相对路径约束（不接收用户裸路径）。
- 段名校验单一权威（`pkg/storage/name.go`）：Windows 保留字（含 `CON.txt` 基名判定）、尾点、尾空格、大小写碰撞、分隔符、超长；租户名、upload_id、文件名段共用。

### 4.3 pkg/quota：通用配额（路径化子池 + 预留句柄）

```go
global := quota.NewPool(cfg.MaxStorageBytes)        // 根池 ""
tenant := global.Scope("/tenant/alice", quotaBytes) // 子池，路径化（http 路由式叠加）
userB  := tenant.Scope("/user")                     // 可继续叠加，后续按功能桶细分配额

res, err := bucket.TryReserve(estimate)   // 预留（预估/占位）
res.Commit(actualSize)   // 按实际占用对账（内部算 estimate→actual diff）
res.Release()            // 放弃预留（失败/取消）
```

- 子池操作自动向父链聚合 → 调用方只感知自己的 bucket，全局兜底在根池自动生效。
- 取代现有 `TryReserve(pre) → Release(pre) → TryReserve(actual)` 三段式对账。
- 命名通用（Pool/Scope/Reservation），不含租户/存储概念。

## 5. 功能模块落盘映射

rel 均相对租户根。

| 功能 | 旧布局 | 新布局 | API 语义 |
|---|---|---|---|
| 用户文件 | `<owner>/<remotePath>` | `user/<remotePath>` | remotePath 不变，相对 user 根 |
| 版本快照 | `<owner>/.__versions__/<p>/<id>` | `version/<p>/<id>` | 不变 |
| 云任务文件 | `.__cloud__/<taskID>/<file>` | `cloud/<taskID>/<file>` | kind=cloud_task filename 仍 `<taskID>/<file>` |
| 云归档 | `.__cloud_archives__/<owner>/<name>` | `archive/<name>` | kind=cloud_archive |
| 分块会话 | `.__chunked__/<owner>/<id>/<chunk>` | `chunk/<id>/<chunk>` | upload_id 不再带 owner 前缀 |
| 云任务状态 | `.__downloads__/<id>.json` | `meta/cloud/<id>.json` | taskID 全局唯一 |
| 云任务组状态 | `.__downloads__/groups/<gid>.json` | `meta/cloud/groups/<gid>.json` | 同 |
| 同步任务状态 | `.__sync__/<id>.json` | `meta/sync/<id>.json` | 同 |
| checksum 缓存 | 全局 `.checksums.json`（key 嵌 owner） | `meta/checksums.json`（key = 相对租户根全功能 rel） | key 无 owner 参数 |

**简化红利**：

- `checksumStoreKey(owner, path)` 的 owner 参数消失；各功能桶 rel 天然互斥（`user/` vs `cloud/` vs `archive/`）→ 版本 key 碰撞、归档与用户文件 key 碰撞从根上消失。
- `ownerScopedSessionKey` / `validateSessionOwner` / `ownerScopedUploadKey` 整套 owner 前缀逻辑删除——租户隔离由目录物理保证，upload_id 用裸 id，防伪造靠"会话只在本租户 `chunk/` 下创建"。
- `internalDirNames` / `isInternalDirPathPrefix` / `isInternalFirstName` / `hasInternalDirAtAnyDepth` 全部删除——用户输入只可能落在 `user/` 前缀内，功能路径由服务端内部构造。

## 6. 守卫与跨平台

- **单入口**：`UserRel(remotePath)`（用户输入）与 `FeatureRel(bucket, sub)`（服务端内部）两个方法；所有 handler 只用它们构造路径，杜绝散落判定。list subdir / archive-dir 守卫缺口随之闭合。
- **段名校验**：`pkg/storage/name.go` 单一权威，覆盖 Windows 保留字（含 `CON.txt`）、尾点、尾空格、大小写碰撞、分隔符、超长。
- **路径归一**：协议路径统一 ToSlash（`user/` 下）；磁盘路径由 os.Root + FromSlash 处理。Windows/Linux 差异收敛在 `pkg/storage`。
- **os.Root**：每个租户一个 `*Root`，所有文件操作相对它；`joinSafePath` 退役。

## 7. 配置变更

```yaml
storage_root: ./storage          # 替换 uploads_dir（默认值 ./storage）
owner_quotas:                    # per-tenant 配额（* = 默认值，缺失租户用默认）
  "*": 5GiB
  anonymous: 1GiB
  alice: 10GiB
max_storage_bytes: 0             # 全局兜底（保留语义：0 = 不限制）
```

- `uploads_dir` 配置键废弃，由 `storage_root` 取代。
- `owner_quotas` 新增；`max_storage_bytes` 保留为全局兜底。
- `LAYOUT_VERSION` 文件在 `OpenRoot` 时校验，未来布局变更提供迁移钩子。

## 8. 迁移阶段

| 阶段 | 内容 | 出口标准 |
|---|---|---|
| P0 | `pkg/quota` + `pkg/store` + `pkg/storage`（os.Root/段名校验/Pool-Scope/Store-JSONStore）纯新代码 | 新包单测全绿 |
| P1 | 装配层：启动 OpenRoot + GlobalPool + 租户 Scope；配置 `storage_root`/`owner_quotas` | 启动 + healthz |
| P2 | 用户文件链路迁移：upload/download/delete/rename/dirs/list/search/share/version → `Tenant` API | 每迁移一个跑通现有 owner 测试 + 新布局测试 |
| P3 | 功能模块迁移：chunked_upload（去 owner 前缀）、cloud_download/cloud_archive（落 `<tenant>/cloud|archive|meta/`）、syncmgr（user 根 + meta/sync） | 对应测试迁移通过 |
| P4 | 配额接线：写路径 TryReserve/Commit 接入 Scope；StorageManager 拆 GlobalPool + 租户 Scope；stats 按租户 | 配额测试 + 统计测试 |
| P5 | 删除旧实现：owner_path.go、`.__xx__` 判定、joinSafePath、checksumStoreKey owner 前缀、ownerScoped* 系列 | 全量测试 + E2E 通过，grep 无旧符号残留 |

策略：新功能完整实现（P0-P1 全部新代码），在保证测试场景完整通过的基础上逐步迁移旧实现（P2-P5），每阶段测试全绿再进下一阶段。

## 9. 测试矩阵与边界场景清单

### 分层

1. **单元**：
   - `pkg/store`：原子写 / 崩溃残留清理 / 前缀遍历 / 泛型编解码。
   - `pkg/storage`：段名校验 Windows 表驱动（保留字含 `CON.txt`/尾点/尾空格/大小写/分隔符/超长）、`UserRel`/`FeatureRel` 边界、os.Root 防穿越（`..` / 绝对路径 / 符号链接逃逸 / 深层嵌套）。
   - `pkg/quota`：父子对账、Reservation Commit/Release diff、超租户上限 / 超全局上限 / 释放回退。
2. **集成**：每个 handler 在新布局下的等价行为——以现有 owner 测试全家桶（safepath / cloud / chunked owner）为功能一致性基线逐功能迁移。
3. **E2E**：真实二进制 + 多租户隔离（alice/bob/anonymous 三租户互相不可见：download/list/search/stats/版本/分享/云任务/分块会话/同步）。

### 边界场景显式清单（逐项打勾）

- **路径**：空 / `.` / `..` / 绝对路径 / Windows 反斜杠 / 深层嵌套 / 符号链接 / 并发重命名。
- **名称**：Windows 保留字（含 `CON.txt`）、尾点、尾空格、大小写碰撞、超长。
- **租户**：空 owner（anonymous）、非法 owner 400 拒绝、跨租户全部不可见（download/list/search/stats/版本/分享/云任务/分块会话/同步）。
- **配额**：预估 vs 实际对账、租户上限、全局兜底、释放回退。
- **恢复**：分块会话重启、云任务续传、同步任务重启。
- **并发**：同文件并发上传、多租户并发。
- **功能联动**：版本开/关、归档已存在 409、断点续传、If-Range 续传。

## 10. 后续扩展（本次不实现，设计预留）

- 存储后端插件：`store.Register("mysql", ...)` / `"redis"` / 对象存储，配置切换。
- 按功能桶细分配额：`tenant.Scope("/user")` / `tenant.Scope("/cloud")` 已在配额 API 中支持，配置可扩展为 `owner_quotas.alice.user: 8GiB`。
- 分享记录、审计日志持久化接入 `pkg/store`。
- 布局版本迁移：`LAYOUT_VERSION` 变更时提供迁移钩子。

## 11. 关联文档

- `docs/architecture.md`（分层传输架构）
- 本次审计报告（2026-08-31，多租户 owner 目录处理独立架构审计）
