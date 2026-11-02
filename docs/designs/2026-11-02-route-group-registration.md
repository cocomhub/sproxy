# 路由注册下放与 URL 命名规范化（设计）

> 状态：设计 + 试点
> 前置：S1–S15 / T1–T3 切片、P1 策略对象（均已交付）
> 作用域：服务端路由装配 + URL 契约（web/CLI 同步改）

## 1. 现状测量（2026-11-02）

| 指标 | 实测 |
|---|---:|
| `pkg/server/routes.go` | 1,006 行，其中 `RegisterRoutes` 占 **147–760（613 行）** |
| 路由注册原语调用 | **231 处**（218 在 routes.go，19 在 routes_setup.go） |
| 去重 `(method, path)` | **124 条** |
| 双 mux 重复注册 | **100 条**（srvMux 122 条 / localMux 115 条） |
| 鉴权分类分布 | tunnel 115 / auth 62 / user 30 / reader 18 / public 12 |
| 内部分组 helper | **0 个**（所有注册内联在一个函数里） |

双注册的固定形态：

```go
localMux.HandleFunc("POST /upload", h.upload)                       // 隧道内层：裸注册（隧道加密即认证）
srvMux.HandleFunc("POST /upload", h.fileRoute(h.upload))            // 外部面：按分类包装
```

问题不是「注册在哪一行」，而是：
1. **613 行单函数 + 231 处内联**，改一条路由要在千行文件里定位、且容易漏改另一半（双注册）；
2. 子包（cloudtasks/share/notify/ai/verify）的路径全部写死在装配层，**新增端点必须改装配根**；
3. 鉴权分类由包装链隐含，只能靠 `route_classes.tsv`（249 行）事后镜像——两份事实源易漂移；
4. URL 命名不一致（见 §4）。

## 2. 目标

1. **每个包管理自己的 URL 子路径**：`pkg/server` 只构造并传入「组 router」，子包在该组上注册 `method + 子路径 + handler`；
2. **鉴权分类仍由装配层决定**（安全属性不下放）：分类是**组**的属性，不是路由的属性；
3. URL 命名规范化，web/CLI（`pkg/client`、`cmd/sclient`、`web/static`）同步调整；
4. 门禁 **R38** 继续 deny-by-default，且分类表有唯一事实源。

## 3. 设计：`pkg/server/router`

### 3.1 类型

```go
// Class 是路由鉴权类别（与 R38 分类表同名）。
type Class string
const (Public, Auth, User, Reader, Tunnel, Admin, Node Class = ..., ...)

// Group 是一组共享「前缀 + 鉴权策略」的路由注册面。
// 分类在构造期由装配层固定，子包只能用，不能改。
type Group struct {
    prefix     string
    srv, local *http.ServeMux
    srvWrap    func(http.HandlerFunc) http.HandlerFunc // nil = 不注册到 srvMux
    localWrap  func(http.HandlerFunc) http.HandlerFunc // nil = 不注册到 localMux
}

func New(srv, local *http.ServeMux, prefix string,
         srvWrap, localWrap func(http.HandlerFunc) http.HandlerFunc) *Group

func (g *Group) Handle(method, path string, h http.HandlerFunc) // 双端注册
func (g *Group) Server(method, path string, h http.HandlerFunc) // 仅外部面
func (g *Group) Local(method, path string, h http.HandlerFunc)  // 仅隧道面
func (g *Group) Sub(prefix string) *Group                       // 继承策略，前缀拼接
func (g *Group) Class() Class
func (g *Group) Prefix() string
```

### 3.2 子包侧

```go
// cloudtasks/routes.go
func RegisterRoutes(g *router.Group, bind func(Op) http.HandlerFunc) {
    g.Handle("POST", "/download", bind(CreateDownload))
    g.Handle("POST", "/download/batch", bind(CreateBatchDownload))
    g.Handle("GET", "/tasks", bind(ListTasks))
    ...
}
```

`bind` 由装配层提供（`h.cloudTaskHandler`，内含 `ActorFrom` 等装配语义——遵循 P1-c 裁决：**ctx→actor 不进领域包**）。

### 3.3 装配侧

```go
cloud := router.New(srvMux, localMux, "/api/cloud", h.authMiddleware, nil)
cloudtasks.RegisterRoutes(cloud, h.cloudTaskHandler)
```

一条事实源：**组 = 前缀 + 分类 + 包装器**。新增端点只改子包一行，装配层零改动（前提是该子包已有组）。

### 3.4 为什么不直接「子包自注册 + 自选包装器」

分类决定「一条路由要不要鉴权、要不要角色门」，是安全边界。让每个包自选分类会：
- 把 deny-by-default 的审计面拆成 19 份（R38 无法再靠一张表通读）；
- 让新增包时最容易犯错的那一步（选错分类）从「装配层一处」扩散到「每包每路由」；
- 与 R38 现行判据（从 wrapper 链**机械提取**分类）不再兼容。

组级策略把「前缀与路径」下放、把「分类」留在装配层——这正是「pkg/server 只传入组 router」的字面实现。

## 4. URL 命名规范（提案，随迁移分批落地）

**规则**

1. **集合用复数名词**：`/api/cloud/tasks`、`/api/shares`、`/api/volumes`、`/api/secrets`；
2. **单例资源用单数**：`/api/config`、`/api/stats`、`/api/health`；
3. **动作作为子资源**（POST + 名词化动词）：`POST /api/cloud/tasks/{id}/cancel`、`POST /api/trash/restore`；
   禁止「集合上的裸动词」新增（`/api/volumes/move`、`/api/volumes/copy` 属历史遗留，迁移时收敛为 `/api/volumes/{name}/move`）；
4. **无 `/api` 前缀的路由只允许三类**：健康检查（`/healthz` `/livez` `/readyz`）、公网分发（`/s/{token}`）、协议挂载（`/dav/` `/s3/` `/ui/` `/tunnel`）；文件面遗留路径（`/upload` `/download` `/delete` `/rename` `/mkdir` `/rmdir`）**保持不动**（外部客户端依赖，且 sclient 已绑定，改名净收益为负）；
5. REST 动词与语义一致：`GET` 只读、`POST` 创建/动作、`PUT` 全量更新、`DELETE` 删除。

**已确认的不一致（可修）**

| 现状 | 建议 | 影响面 |
|---|---|---|
| `POST /api/share`（单数，创建）+ `GET/DELETE /api/shares/{token}`（复数） | 统一为 `/api/shares` | server + pkg/client + sclient + web + tests |
| `POST /api/volumes/move`、`/copy`、`/rebalance`、`/refresh` | `/api/volumes/{name}/move` 等 | 同上（sclient volume 子命令） |
| `POST /api/ai/privacy/purge` | 保留（动作子资源，合规） | — |
| `POST /api/notify/test` | `/api/notify/channels/{name}/test`（可选） | web 通知页 |

**迁移策略**：URL 改动单独成 PR，**同一 PR 内同步** `pkg/client`（Go SDK）、`cmd/sclient`、`web/static`、`docs/api.md`、`route_classes.tsv` 与相关测试；不保留旧路径 alias（避免长期双份）；破坏性变更按 `feat(cli)!`/`BREAKING CHANGE` 标注。

## 5. 门禁变更（R38）

- 扫描面从「routes.go 的 wrapper 链」改为「`router.New(...)` 构造点的 class + 各组注册的 `(method, path)`」；
- `route_classes.tsv` 保留为**期望表**，由测试在运行期构建实际路由表后比对（不再解析源码行号）；
- 新增子包时若忘记在装配层建组，路由表里就没有它的条目 ⇒ 表比对红。

## 6. 性价比评估

| 项 | 收益 | 成本 | 结论 |
|---|---|---|---|
| **组 router 内核** | 双注册一次写完；分类单点 | ~150 行 + 测试 | **做**（试点） |
| **子包路由下放（cloudtasks 32 条）** | 装配根减少 ~64 行；新增端点零改装配 | 子包 +40 行、装配 -64 行 | **做**（试点） |
| 其余子包（share/notify/ai/verify，~23 条） | 同上，比例递减 | 每组 1 PR | 后续分批 |
| files/volumes 路由（~90 条） | 最大块，但 handler 全在装配层 | 需先切片，收益延期 | **暂不做** |
| URL 规范化（share 单复数、volumes 动作） | 契约一致性 | 4 个消费者同改 + 破坏性变更 | **单独 PR**，优先级低于注册下放 |
| 全量 URL 重命名（文件面 `/upload` 等） | 无实质收益 | 破坏外部客户端 | **不做** |

## 7. 交付判据

- 试点后：`/api/cloud` 的 16 条路由声明在 `cloudtasks` 包内，装配层只保留组构造 + 一次 `RegisterRoutes`；
- `RegisterRoutes` 长度按批次下降（试点目标 −60 行）；
- R38 改为运行期路由表比对，仍是 deny-by-default；
- 每 PR：`go test ./pkg/server/... ./internal/archcheck/`、`make lint`、`make build-all` 全绿，文档同步。
