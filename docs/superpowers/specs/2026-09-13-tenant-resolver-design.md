# 租户解析下沉 设计

> 本规格解决 file-service 抽取工作留下的第 1 项遗留：租户「懒创建 + 缓存 + 失败关闭」这层策略在 `pkg/server` 与 `pkg/volume/registry` 各写一份，`owner` 规范化在两处各写一份。
> 实施见 `docs/superpowers/plans/2026-09-13-tenant-resolver.md`（T1 / T2）。

**目标**：把租户解析的**公共骨架**下沉到 `pkg/storage`（存储布局域），使 `pkg/files` 的 `TenantResolver` 由 `pkg/storage` 的实现**结构上直接满足**，装配层不再需要适配方法；同时把「同一份逻辑有两个副本」收敛为一份。

---

## 1. 实测证据（现状）

| # | 事实 | 位置 |
|---|---|---|
| 1 | **创建骨架两份，逐字同构** | `pkg/server/handlers.go:266-316`（`h.tenantFor`）与 `pkg/volume/registry/set.go:140-176`（`Set.Tenant`）——均为 `ValidateSegmentName → parent.Abs → MkdirAll → OpenRoot → NewTenant`，失败即关闭已开句柄并返回 nil |
| 2 | **已有行为漂移** | `h.tenantFor` 预建 `meta` 桶（校验和/凭据要写）；`Set.Tenant` **不**预建（注释说明「meta 桶仅在默认卷权威」）。同构代码已各自演化 |
| 3 | **缓存/锁两份** | `Handlers.tenantRoots map + tenantMu`（键 = owner）与 `Set.tenants map + tenantMu`（键 = `vol\x00owner`） |
| 4 | **owner 规范化两份** | `pkg/server/handlers.go:250-259` 与 `pkg/files/service.go:284-295`；目前靠 `helper_impl_drift_test.go` 的 parity 守卫硬扛 |
| 5 | **关闭逻辑两份** | `Handlers.Close`（遍历 `tenantRoots`）与 `Set.Close`（遍历 `tenants`） |
| 6 | **消费者** | `h.tenantFor`/`h.tenantOf`/`h.volumeTenant` 在 `pkg/server` 有 **44 处**调用点，分布在 13 个文件 |

> 证据 2 是关键：两份实现已经**不是**逐字相同了。这不是「重复但等价」，而是「重复且已分叉」——每修一次租户创建（如新增桶预建、句柄泄漏修复）都要记得改两处，且**没有任何机械约束**。

## 2. 目标与非目标

### 目标
1. 租户创建的**公共骨架**单源在 `pkg/storage`。
2. 租户**缓存 + 锁**可由 `pkg/storage` 提供，`pkg/server` 与 `pkg/volume/registry` 复用之。
3. `owner` 规范化与 `anonymous` 常量单源。
4. `pkg/files.TenantResolver` 由 `*storage.TenantCache` 结构上满足 ⇒ 删掉 `filesRuntime.TenantFor` 适配方法。

### 非目标
- **不改租户磁盘布局**（`<root>/<owner>/` 与各功能桶）。
- **不改默认卷预建 `meta`、非默认卷不预建**的既有语义。
- **不改 HTTP 契约**（状态码、错误文案）。
- **不改类型名**（`Root`/`Tenant` 保留原名）——只新增，不重命名。

## 3. 目标设计

### 3.1 `pkg/storage/tenant_resolve.go`（新增）

```go
// ---- 单源常量与规范化（替换两份私有副本）----

// AnonymousOwner 是未认证请求的默认租户名。租户名是存储布局契约（<root>/<owner>/…），
// 该值在 pkg/server、pkg/files 与磁盘布局之间必须一致。
const AnonymousOwner = "anonymous"

// NormalizeOwner 把空 owner 归一为 AnonymousOwner。
func NormalizeOwner(owner string) string

// ---- 创建骨架：只做「创建」，不做缓存 ----

// OpenTenant 在 parent 下按 owner 打开（必要时创建）租户子根。步骤逐字保留既有语义：
//
//	ValidateSegmentName(owner) → parent.Abs(owner) → MkdirAll → OpenRoot → NewTenant
//
// 任一步失败即 fail-closed：返回 (nil, err)，并关闭该步之前已打开的句柄
// （绝不泄漏 *Root，也绝不回落 parent）。owner 归一化由调用方负责（见 NormalizeOwner）。
//
// 选项：
//   WithMetaBucket() 预建 meta 桶（默认卷权威；非默认卷不预建，语义保留）
//   WithLogger(l)    逐级 Warn 日志（nil 回落 slog.Default()）
//
// 错误消息保留既有文案（"非法租户名"/"租户路径越界"/…），日志级别与字段一致。
func OpenTenant(parent *Root, owner string, opts ...TenantOption) (*Tenant, error)

// ---- 缓存 + 锁：单父根版本 ----

// TenantCache 是「单父根 + 按 owner 缓存租户」的并发安全缓存（替代 Handlers.tenantRoots
// 与 Set.tenants 的同构实现）。零值不可用。
type TenantCache struct{ /* mu sync.Mutex; parent *Root; tenants map[string]*Tenant; opts []TenantOption */ }

func NewTenantCache(parent *Root, opts ...TenantOption) *TenantCache

// TenantFor 返回 owner 的租户；parent 为 nil / owner 非法 / 创建失败一律返回 nil
// （fail-closed，调用方按 400 处理）。**方法集与 files.TenantResolver 一致**：
// 装配层可直接 files.New(cache, ...)，无需适配。
func (c *TenantCache) TenantFor(owner string) *Tenant

// Close 关闭**本缓存拥有的**租户子根（不关 parent——parent 由创建者负责）。
// 幂等；关闭后缓存清空（再 TenantFor 会重新创建，语义与现状 Handlers.Close 后一致）。
func (c *TenantCache) Close() error

// ---- 磁盘扫描（listTenantIDs 的下沉）----

// ListOwners 扫描 parent 下的直接子目录，返回合法租户名（按名排序）。跳过遗留
// `.__` 内部目录与非法段名目录（逐字保留既有过滤语义）。
func ListOwners(parent *Root) []string
```

### 3.2 两处消费方

| 消费方 | 现状 | 目标 |
|---|---|---|
| `pkg/server.Handlers` | 字段 `globalRoot` + `tenantRoots` + `tenantMu`；方法 `tenantFor`（~50 行）与 `listTenantIDs`（~35 行） | 字段 `tenants *storage.TenantCache`；`tenantFor(owner)` 保留为**一行薄转发** `return h.tenants.TenantFor(owner)`（44 处调用点零 diff）；`listTenantIDs` 一行转发 `storage.ListOwners(h.globalRoot)` |
| `pkg/volume/registry.Set` | 字段 `tenants map[string]*Tenant` + `tenantMu`；方法 `Tenant(volName, owner, log)` 体 ~40 行 | 字段 `caches map[string]*storage.TenantCache`（每卷一个，键仍含卷名以避免跨卷句柄混用）；`Set.Tenant` = 查 `caches[volName]` → `TenantFor(owner)`；`Set.Close` 遍历 `caches` 关闭 |

### 3.3 `pkg/files` 侧

- `anonymousOwner`/`normalizeOwner` 改为委托 `storage.AnonymousOwner`/`storage.NormalizeOwner`（保留私有别名以免改 20 余处调用点，或直接替换——由实施者按 `go build` 报错量决定，见计划 T1 步骤）。
- `pkg/server` 装配改为 `files.New(h.tenants, ...)`，删除 `filesRuntime.TenantFor`。
- `helper_impl_drift_test.go` 中 `normalizeOwner` 的 **parity 守卫改为「两包都委托 storage」的委托守卫**（同源后 parity 已无意义，但「不许重新内联」仍需守卫）。

## 4. 分层与门禁影响

- `pkg/storage` 是 **L0/L1 基础包**，新增能力不引入任何新的 `pkg/*` 依赖边（`pkg/storage` 现状零 pkg 内部依赖，实测）。
- `pkg/storage` 新增 `log/slog`（标准库）依赖——**已确认接受**：否则逐级 fail-closed 告警文案要上抛到调用方，日志语义会变。
- `pkg/storage` 不得导入 `pkg/quota`/`pkg/volume`（那会把它拽出基础层）；`TenantCache` 只依赖 `*Root`。
- `registry.Set` 已登记 L2，`pkg/storage` 登记 L0 ⇒ `Set → storage` 仍是低层方向，合法。

## 5. 迁移切分

| 片 | 内容 | 可独立验证点 |
|---|---|---|
| **T1** | `pkg/storage` 新增 `AnonymousOwner`/`NormalizeOwner`/`OpenTenant`(+Option)/`ListOwners`；`pkg/server.tenantFor`/`listTenantIDs` 与 `pkg/files` 改引用（**行为逐字不变**，缓存仍留在原处） | 新增 `tenant_resolve_test.go`（失败路径 fail-closed、句柄不泄漏、meta 选项开/关、`ListOwners` 过滤）；四条机械核对；e2e（多租户布局） |
| **T2** | `storage.TenantCache`；`registry.Set` 改用每卷 cache；`Handlers` 改持 `h.tenants`（删 `tenantRoots`）；`files.New(h.tenants)` 并删 `filesRuntime.TenantFor`；漂移守卫改委托守卫 | 同 T1；`registry` 用例名守恒；`files`/`server` 全量测试；覆盖率零覆盖函数仍为 0 |

> T1 与 T2 分开的理由：T1 只新增能力 + 改引用（**缓存不动**，回归面最小）；T2 才动缓存与关闭顺序（需要 e2e 覆盖多卷 + 关闭路径）。两者各自可回退。

## 6. 行为不变的硬约束（实施与审查清单）

1. 租户磁盘路径：`<root>/<owner>/`，功能桶名不变（`user/cloud/archive/chunk/version/meta`）。
2. 默认卷（唯一根 / `volSet.Default()`）**预建 `meta`**；非默认卷**不预建**。
3. `owner == ""` ⇒ `anonymous`。
4. 失败路径 fail-closed：非法段名 / 路径越界 / MkdirAll 失败 / OpenRoot 失败 / NewTenant 失败 ⇒ 返回 nil，调用方 400，**绝不回落父根**。
5. 不泄漏句柄：`OpenTenant` 在 `NewTenant`/meta 失败时必须关闭刚打开的 `*Root`。
6. `Close` 顺序：先关租户子根、后关卷根（`volSet.Close()`）；`TenantCache.Close` **不关** parent。
7. 日志：级别（`Warn`）与既有字段名（`owner`/`volume`/`error`）不变——e2e 与日志断言可能依赖。

## 7. 风险与取舍

| 风险 | 处置 |
|---|---|
| 关闭顺序变化导致双重 Close / 提前 Close | `TenantCache.Close` 只关自己的租户子根；`Handlers.Close` 的调用顺序保持「租户 → 卷集合」；e2e 覆盖优雅停服 |
| `Set.Tenant` 的日志参数（`log *slog.Logger`）被调用方按次传入（可能每次不同） | `TenantCache` 在**创建时**固定 logger；`Set.Tenant(volName, owner, log)` 签名保留，首次为某卷创建 cache 时用该次入参，后续忽略（与现状「首次创建才记日志」一致）。**注意这是行为近似，不是逐字不变**：`volume` 字段经 `WithLogAttrs("volume", volName)` **保留**，但告警**文案**由「拒绝在卷上创建」变为域中性的「拒绝创建」（级别 `Warn` 与字段名不变）。经实测，全仓无任何测试断言这些文案 |
| 44 处调用点 | 保留 `h.tenantFor` 一行转发，调用点零 diff |
| `pkg/storage` 从「纯布局」变成「布局 + 策略」 | 边界仍清晰：策略只关乎**租户根的生命周期**，不涉及文件操作、配额、卷 ACL；`pkg/files` 仍只依赖 `pkg/storage` 顶层包 |

## 8. 未做（记录理由）

- **不把 `TenantCache` 做成泛型 `Cache[K,V]`**：只有一种元素类型，泛型只增加阅读成本。
- **不把 `volSet`/卷 ACL 一起下沉**：卷是另一个域（已登记 L2 子包），租户缓存只是「按卷分片」，不引入卷概念。
- **不改 `registry.Set.Tenant` 的公开签名**：避免牵动装配层与测试。
