# 路由分类单一事实源（T4 阶段三）

> 状态：方案已自审通过，分批实施。
> 前置：`2026-11-02-route-group-registration.md`（T4 机制落地）、R38/R55 门禁。

## 1. 问题（实测）

装配层一条**文件域路由**目前最多在 **5 处**声明/分类：

| # | 位置 | 缺失后果 |
|---|---|---|
| 1 | `srvMux.HandleFunc(... h.fileRoute\|h.fileRouteRead(h.x))` | 主 mux 面 `fileRoute` 判「不在文件组」⇒ 500（**响亮，fail-closed**） |
| 2 | `localMux.HandleFunc(...)` 裸注册 | 无（注册遗漏 ⇒ 404，响亮） |
| 3 | `isFileGroupedRoute` 白名单 | 隧道内层 `localMuxGate` **不做任何角色检查**⇒ 静默越权（**silent fail-open**） |
| 4 | `isReadOnlyFileRoute` 白名单 | reader 越权写 / user 被拒读（**silent，权限错配**） |
| 5 | `route_classes.tsv`（每 mux 一行） | R38 红（响亮） |

实测：`fileRoute` 28 处 + `fileRouteRead` 17 处 = 45 条文件域路由；`localMux` 83 条裸注册；
`route_classes.tsv` 255 行（tunnel 121 / auth 66 / user 30 / public 20 / reader 18）。

**近一半的分类事实是 silent-fail 的**（#3 / #4 漏列不会红，只会错）。注释本身已在警告：
「漏列 → fileRoute 判定「不在文件组」500，防接线错误静默放行」——说明作者已知风险，但用「列表 + 测试兜底」缓解，而不是消除。

## 2. 方案：分类声明一次，两侧行为派生

`pkg/server/router` 已具备全部机制（T4 / #1002）：

```go
router.Route[H]{Method, Path, Server Class, Local Class, H}
router.Register(srvMux, localMux, prefix, routes, bind, wrap)  // 两侧分别按类取包装器
routeWrapper(class)                                            // 未知类 ⇒ panic（fail-closed）
```

文件域路由迁移后**只有一处声明**：

```go
{Method: "POST", Path: "/upload", Server: router.ClassUser, Local: router.ClassTunnel,
 H: (*Handlers).upload}
```

- `Server: ClassUser` ⇒ `routeWrapper` 给出 `h.fileRoute`（写子组 requireRole(user)）；
- `Local: ClassTunnel` ⇒ 裸注册（隧道内层由 `localMuxGate` 按类收口）；
- `localMuxGate` 改为查**注册期构建的 class map**（`"METHOD PATH" → Class`），不再是字符串白名单；
- 三个谓词（`isFileGroupedRoute` / `isReadOnlyFileRoute` / `isWriteFaceRoute`）在**全部路由进表后删除**。

### 与既有门禁的关系

R38 已有四个扫描面（其中一个是子包数据表）。本方案**新增双向锁**：

- 生产侧：表里声明的 `Server`/`Local` class；
- 审计侧：`route_classes.tsv` 的行；
- 门禁断言：**逐条相等**。改分类必须同时改两处 ⇒ diff 可见，无法静默漂移。

这样 TSV 从「由 wrapper 链机械提取的快照」升级为「与实现互为约束的审计记录」。

## 3. 「由子包自行注册 handler」的可行性与性价比（用户提问）

结论：**不做**。理由分三层：

1. **handler 需要装配态**：`h.` 上有 actor、审计、配额、写面门、`deps.go` 的 12 组依赖；
   子包持有 handler 就等于要么持有一大票能力接口（P1-c 已证明 40 个方法才够 cloudtasks），
   要么反向 import 装配层（R6 禁止）。
2. **安全决策必须留在装配层**：class → wrapper 的映射是「谁能访问」的裁决，分散到 20 个子包
   会把 deny-by-default 的审计面碎成 20 份，R38 再也无法单点校验（这条已在 T4 设计文档里定为红线）。
3. **收益已经拿到**：真正的重复不是「handler 住哪」，而是「同一分类事实写 5 遍」。数据表方案
   能消除后者，且不引入任何跨包耦合。子包已拥有 5 张路由表（cloudtasks/share/notify/ai/verify），
   恰恰因为它们的 handler 本来就在子包（Service）。**表跟着 handler 走，而不是反过来**。

## 4. 分批计划

| 批次 | 内容 | 行为变化 |
|---|---|---|
| B1 | 文件域 45 条进表（`pkg/server/routes_files.go`），两侧由 `Register` 派生；新增 class map + 差分等价测试（新表 vs 旧谓词逐条断言）。**已交付**：`router.Register` 支持空前缀（根组）；R38 新增分类双向锁；差分测试抓到 `GET /api/search/semantic` 两面策略不一致的真实缺陷 | 无（谓词仍在）+ 附修鉴权缺陷 |
| B2 | 同步 / 回收站 / 运维管理面进表。**已交付**（#1023、#1024） | 无 |
| B3 | 其余域进表。**已交付**：同步/回收站/运维（#1023、#1024）、**credentials（含 Guard 机制，本轮）** | 无（同时修掉一个 P1：隧道内层凭据端点因限流器注册顺序 panic） |
| B4 | **文件域**翻转：`localMuxGate` / `writeGuardMiddleware` / `fileRoute[Read]` 改查 index（键 = `mux.Handler(r)` 的匹配模式）；删除 `isFileGroupedRoute` / `isReadOnlyFileRoute`；R38 保持「TSV ↔ 声明」双向锁。**已交付** | 有（对未声明模式 fail-closed） |

剩余：`isWriteFaceRoute` 的**非文件**写面清单（PUT /api/config、凭据/cloud/sync/volumes 写族、
notify test、ai purge、verify、S3 写）仍是显式列表——它只影响集群只读副本的写门，未随本次
翻转删除；待剩余域（含 credentials）全部进表后一并收敛。

## 4.1 实施要点（B4 实测）

1. **`r.Pattern` 在包住 mux 的中间件里是空的**：模式由 ServeMux 在调用 handler 时才写入。
   中间件层必须用 `mux.Handler(r)`（只匹配、不执行）拿同一个模式串——否则查表永远 miss，
   表现为「文件域全部放行」（首次实现即踩到，被 `TestRBAC_TunnelInnerReaderWriteForbidden` 抓住）。
2. **索引必须包含子包声明的文件域路由**（`share.Routes()`：创建/撤销 ClassUser、列表
   ClassReader），否则 `fileRoute` 自检把 `/api/shares` 判成「不在文件组」⇒ 500
   （被路由冒烟门禁 #1021 抓住）。
3. 表驱动比旧前缀匹配**更精确**：旧 `isReadOnlyFileRoute` 用 `HasPrefix("/api/shares/")`
   把不存在的 `GET /api/shares/{token}` 也判为只读；新索引用模式串，不存在即不命中。

## 5. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 表里 class 与旧谓词不一致 ⇒ 行为漂移 | 差分测试逐条断言（B1 起就红） |
| `Register` 对两侧套同一 wrapper ⇒ local 面双重鉴权 | 显式声明 `Local: ClassTunnel`（wrapper = nil），语义不变 |
| `localMuxGate` 改查 map 后漏声明路由被拒 | 翻转前要求 100% 路由进表；门禁断言「注册总数 == 表项总数」 |
| 大批量机械改动引入手误 | 按域分批，每批跑 RBAC 双测（reader/user/无身份矩阵） |

## 6. 验收标准

1. `git grep -c 'isFileGroupedRoute\|isReadOnlyFileRoute\|isWriteFaceRoute'` == 0；
2. 一条文件域路由只在**一处**声明（表），`routes.go` 中不再出现手工配对的两个 mux 注册；
3. R38 新增断言：表声明 class 与 `route_classes.tsv` 逐条相等（双向）；
4. 变异验证：改一条路由的 class（表内）⇒ RBAC/审计测试红；
5. 既有行为测试断言零修改。

## 7. Guard 机制（credentials 域引入，本轮）

分类（Class）只表达「基础鉴权」，不表达「同一分类内的额外入口控制」。三个公开凭据入口
（register/nonce/login）各自挂独立限流器 + 主 mux 的 IP 门，此前只能靠 20 处内联注册
手工重复；表化需要一个新的表达维度：

```go
{Method: "POST", Path: "/login", Server: router.ClassPublic, Local: router.ClassTunnel,
 Guard: guardCredentialLogin, H: (*Handlers).loginCredentialHandler}
```

- `router.Wrapper` 签名改为 `func(Class, string) ...`，`Route` 增加 `Guard string`；
- Guard 记号在装配层唯一映射（`guardWrapper`）：未知记号 **panic**，限流器未初始化也 **panic**
  （fail-closed）；
- 分类与 Guard 的分工固定为：Class = 谁能访问，Guard = 额外入口控制。

**变异暴露的盲区（已补断言）**：删掉 login 的 `Guard` 后，分类仍是 public、R38 仍绿、
全部凭据测试仍通过——即「公开端点裸奔」无人发现。新增
`TestCredentialRoutes_PublicEntriesCarryGuards` 锁住三个公开入口的 Guard 与预期一致。

**顺带修掉的 P1**：三条公开入口在 localMux 注册时引用**尚未初始化**的限流器字段
（注册在初始化之前），方法值捕获 nil 指针 ⇒ 隧道模式请求 panic 断连（客户端只见 EOF）。
表化把「初始化 → 注册」顺序固定，`guardWrapper` 对未初始化限流器启动期即 panic。
