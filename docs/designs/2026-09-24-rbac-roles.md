# 设计：RBAC 角色细分（RoleReader 只读档位）

## 背景与目标

现状（源码事实）：`pkg/accesskey/accesskey.go` 定义三档角色 `RoleUser`/`RoleNode`/`RoleAdmin`
（`Key.Role` 字段持久化进 credentials.json，空值由 Replace/RingAuthenticator 归一 user）；
`pkg/server/auth.go` 的 `requireRole(principal, minRole)` 门禁：minRole=user（文件组）放行
{user, admin}、minRole=node 放行 {node, admin}、minRole=admin 仅 admin、未知 minRole
fail-closed 403；`routes.go` 的 `fileRoute`（主 mux）与 `localMuxGate`（隧道内层）对
`isFileGroupedRoute` 全组成员统一挂 `requireRole(user)`。

目标：新增第四档 `RoleReader`（只读账号：可 GET/list/search/stat/download，禁一切写），
写端点维持至少 user、管理端点维持 admin；现有 user 语义零变化（可读可写）。

## 组件与接口

1. `pkg/accesskey/accesskey.go`
   - 新增 `RoleReader Role = "reader"` 常量（紧随 RoleUser）。
   - 持久化与归一：`Key.Role` 天然支持新值（json 字符串）；空值归一仍为 user
     （reader 是非空显式值，不受 Replace/RingAuthenticator 归一影响，零迁移）。
2. `pkg/server/auth.go` — `requireRole` 扩展：
   - 新增 `case string(accesskey.RoleReader)`：放行 {reader, user, admin}，拒绝 node。
   - 角色层级：reader < user < node < admin（admin 全组放行不变；node 仍不进文件组）。
   - 其余分支（user/node/admin/未知 fail-closed）保持不变。
3. `pkg/server/routes.go` — 文件组按读写拆分（单一事实源防漂移）：
   - 新增 `isReadOnlyFileRoute(path)`（只读子组），与 `isFileGroupedRoute` 同文件同风格：
     GET /download、GET /api/files、HEAD /api/files/stat、GET /api/files/search、
     GET /download/chunk、GET /api/versions、GET /api/archive-dir、GET /api/backends、
     GET /api/volumes、GET /api/volumes/user、GET /api/shares、GET /api/shares/{token}
     前缀组。其余文件组成员 = 写子组（upload/delete/rename/mkdir/rmdir/batch*/archive/
     versions-restore/share-create/volumes-*写操作/backends-presign/trash-*等）。
   - `fileRoute(handler, minRole)`：签名扩展为带 minRole 参数（默认 user）；`fileRouteRead`
     包装只读子组走 `requireRole(p, RoleReader)`，原 fileRoute 写子组保持
     `requireRole(p, RoleUser)` 不变。**只读子组判 403 前仍过 authMiddleware**（认证不变）。
   - `localMuxGate` 同源拆分：路径命中只读子组 → requireRole(reader)；命中写子组 →
     requireRole(user)；principal nil（xfer 直连）语义不变。
4. 凭据管理（`register_handler.go` / 既有 `/api/credentials` handler）：
   - 注册（register）仍恒产 RoleUser（首注册 admin 原子授予不变）。
   - `POST /api/credentials`（akAddHandler）增加可选 `role` 字段（仅 admin 可设 reader/
     user；设 admin 拒绝——同一 ring 无法新增 admin 的既有红线保持），由调用方
     Principal 判定 admin。此为管理面小改，属本功能的收尾片。

## 数据流

1. reader 凭据创建：admin 登录 → POST /api/credentials {owner, role:"reader"} →
   ring 写入 Key.Role="reader" → persistCredentials()。
2. 读请求：sclient/Web 携带 SproxySig → authMiddleware 验签 → RingAuthenticator 读
   Key.Role="reader" 填 Principal.Role → 只读路由 requireRole(reader) 放行 →
   handler 落桶（按 Principal.AK）。
3. 写请求（reader）：同一认证 → 写路由 requireRole(user) 拒绝 → 403（errForbidden）。
4. 隧道路径：外层认证放行 → localMuxGate 按只读/写子组同判（reader 读放行、写 403）。

## 错误处理

- principal nil（未认证）→ 401（errUnauthorized），写/读一致。
- 角色不足（reader 写、node 任一文件操作、user 管理操作）→ 403（errForbidden）。
- 未知 minRole → fail-closed 403（requireRole default 分支，不变）。
- 接线错误：只读子组路由漏挂/误挂 → 500 + Error 日志（isReadOnlyFileRoute 同
  fileRoute 的 fail-closed 断言）。

## 测试 + 变异点

- 测试（httptest + 真实 ring）：
  1. reader 禁写：POST /upload（及 delete/rename/mkdir/batch）→ 403；
  2. reader 可读：GET /download、GET /api/files、HEAD stat → 200；
  3. user 不受影响：读写均 200（回归）；admin 全组放行（回归）；node 文件组 403（回归）；
  4. 空 Role 归一 user（R3-M4 回归）；api_keys 恒 user 语义不变（回归）；
  5. 隧道内层：reader 经 /tunnel 读 200、写 403（localMuxGate 拆分回归）。
- 变异点（改后必须红）：① requireRole 删掉 reader 放行分支 → reader 读 403（测试 2 红）；
  ② 写子组误用 requireRole(reader) → reader 写放行（测试 1 红）；③ isReadOnlyFileRoute
  漏列 /api/files → reader list 403（测试 2 红）；④ 只读子组误挂 user → reader 读 403。

## 片划分

- 片 1（核心）：RoleReader 常量 + requireRole 扩展 + isReadOnlyFileRoute 拆分 +
  fileRoute/localMuxGate 双面接线 + 上表测试与变异验证。
- 片 2（管理面）：/api/credentials 增 role 字段（admin 限定）+ 文档（docs/cli.md、
  config.md 角色段）+ R15 文档门禁同步。

## 风险与零回归保证

- 零回归面：写子组 requireRole(user) 一字不改；user/admin/node 语义全部不变；
  api_keys（Permission read/write）维持独立特性不并入 reader（避免双轨语义纠缠）。
- 风险 1：只读子组清单漂移（新增 GET 路由漏列 → 误归写组 → reader 被拒）——
  由 isReadOnlyFileRoute 单一事实源 + 测试 5 的清单回归（TestLocalMuxCoversAllTunnelRoutes
  同款枚举测试）兜住。
- 风险 2：reader 账号若被授予 SK 仍可开 /tunnel（隧道面只验签不查角色）——隧道
  内层 localMuxGate 已按子组拆分收口，reader 写面 403，风险闭合。
- 风险 3：旧 credentials.json 无 role 字段 → 归一 user（reader 是显式值，无迁移面）。
