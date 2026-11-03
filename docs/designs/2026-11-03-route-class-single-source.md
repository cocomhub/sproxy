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
| B2 | 卷运维 / secrets / backends / 上传会话归入同一文件域表（同批可拆） | 无 |
| B3 | 其余域（cloud/hub/audit/admin/sync/…）进表 | 无 |
| B4 | `localMuxGate` / `writeGuardMiddleware` 改查 class map；删除三个谓词；R38 增加「TSV ↔ 声明」双向锁 | 有（fail-open → fail-closed，仅对未声明路由） |

B1–B3 是纯增量（不改变运行时行为），B4 才是翻转点，且翻转前已有差分测试证明等价。

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
