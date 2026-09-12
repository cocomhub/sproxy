# 文件服务接缝：接口化 + Option 构造 设计

> 本规格记录 `pkg/files` 接缝从「19 字段 `Deps` + 15 项必填」改为「唯一必需项 + 能力接口 + Option」的设计与取舍。
> 实施见 PR #196（Option 入口）、#197（装配层迁移 + 删兼容层）、本片（文档收口）。

**目标**：让文件服务「用多少能力就注入多少」——最小使用只需一个租户解析；额外能力（多卷、配额、台账、分块、版本、审计、计量、锁池、下载路径解析）通过 Option 注入接口，未注入的回落内建默认。

---

## 1. 目标 API

```go
// 唯一必需项放在编译期：TenantResolver（决定请求落到哪个存储根）
svc, err := files.New(resolver)

// 额外能力通过 Option 注入接口
svc, err := files.New(resolver,
    files.WithVolumes(vr),        // 多卷：卷集合 + 卷租户 + 读定位 + 写路由（一个能力接口）
    files.WithQuota(qs),          // 配额 Scope 解析
    files.WithChecksumLedger(l),  // per-tenant 校验和台账
    files.WithChunkedUploads(cu), // 分块会话存储 + 容量回退预留 + 分块大小
    files.WithVersioning(v),      // 版本启停 + 保留上限
    files.WithAudit(a), files.WithMetrics(m), files.WithFileLocks(fl),
    files.WithDownloadPaths(dp), files.WithActor(ar),
    files.WithLogger(fn),         // 取用函数（日志热更新）
    files.WithChunkSize(fn),      // C2：独立可覆盖的纯配置项
)
```

## 2. 能力接口（消费者定义，原子粒度）

| 接口 | 方法 | 未注入时的默认 |
|---|---|---|
| `TenantResolver` | `TenantFor(owner)` | **必需**（`New` 第一参数） |
| `ActorResolver` | `Actor(*http.Request)` | 恒匿名（anonymous 租户） |
| `VolumeRouter` | `Volumes()` / `Tenant()` / `Locate()` / `Route()` | `singleVolume`（由 `TenantResolver` 派生；`Volumes()==nil` = 单卷零回归） |
| `QuotaScopes` | `ScopeFor(owner, rel)` | nil（不记账） |
| `ChecksumLedgers` | `ChecksumStoreFor(owner)` | nil（下载/stat 仍实时算 checksum 头） |
| `DownloadPaths` | `Resolve(*http.Request)` | 仅普通文件；云端 kind → 404 |
| `FileLocks` | `TryMark(owner, rel, value)` / `Acquire(owner, rel)` | 内建 `mapFileLocks`（非阻塞 `sync.Map`） |
| `ChunkedUploads` | `UploadStoreFor(owner)` / `Capacity()` | nil（分块端点按既有 nil-store 语义回包，不 panic） |
| `Versioning` | `Enabled()` / `MaxVersions()` | 关闭、不清理 |
| `Auditor` | `Record(ctx, action, object, result, detail)` | nil（不审计） |
| `Metrics` | `RecordUpload/RecordDownload/RecordDelete` | nil（不计量） |

**方法命名按消费方**：`ChecksumStoreFor` / `UploadStoreFor`（而非都叫 `StoreFor`）——同名不同返回类型会让同一个适配类型无法同时实现两个接口；改名后 `pkg/server` 的单个 `filesRuntime` 即可实现全部能力接口。

## 3. 配置原子化

- **C1 配置随能力走**：`ChunkedUploads` 自带容量回退预留（`Capacity()`）与分块大小；`Versioning` 自带启停与保留上限。配置不再有孤儿接缝项。
- **C2 独立可覆盖**：`WithChunkSize(func() int64)` 允许只覆盖分块大小，不触碰分块会话能力。

## 4. nil 语义（typed-nil 的消除）

「未装配」由三层表达，装配层不再需要 `if x != nil { deps.Field = x }`：

1. **runtime 访问器**（`pkg/files/runtime.go`）：`quotaScope` / `checksumStore` / `uploadStore` / `storageManager` 等对 nil 能力返回 nil；
2. **内建默认实现**：`singleVolume`（多卷）、`mapFileLocks`（锁池）、`disabledVersioning`（版本）、`anonymousActor`（主体）、`defaultDownloadPaths`（下载解析）；
3. **装配层适配器内部**：`filesRuntime.Volumes()` / `Capacity()` 判 `h.volSet` / `h.storageMgr` 是否为 nil 并返回 nil 接口。

## 5. 迁移与验证

| 阶段 | PR | 内容 |
|---|---|---|
| M1 | #196 | 新增 `New` + Option + 内建默认；`Deps`/`NewService` 暂时保留（Deprecated，行为逐字不变） |
| M2 | #197 | `pkg/server` 装配迁移到 Option（`filesRuntime`）；删除 `Deps`/`NewService`/`requiredDeps`/`runtimeFromDeps`；测试基座迁移 |
| M3 | 本片 | 文档收口（包文档改述能力接口与 Option；本规格） |

**每片的可执行验证**：四条机械核对（`test/` 零改动、用例名零丢失、路由表逐条一致、分层门禁 PASS）+ `make lint` / `lint-all` 0 issues + `go build ./...` / `make build-all` + `go test ./pkg/... ./internal/...` + `-race` + `make test-e2e`。

## 6. 未做（独立议题）

- **可复用 `storage.NewTenantResolver`**（供嵌入方一行构造）：会与 `pkg/server.tenantFor` 形成第二份租户懒建实现（含 `globalRoot` fail-closed、meta 桶预建、缓存与告警），需先评估是否把 `tenantFor` 反向委托过去；不在本工作范围。
- **`pkg/server` 其余职责的接缝化**（cloud / auth / share / sync / stats…）：按同一「能力接口 + Option」模式逐域评估。
