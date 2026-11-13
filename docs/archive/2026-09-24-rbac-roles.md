<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# RBAC 角色细分（RoleReader 只读档位，11.5-①）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-rbac-roles.md` 已于 2026-11-13 归档。
> role 取值全集与生效矩阵以 `docs/api.md`（§凭据管理 ·「账号角色（role）」）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

此前只有三档角色（`user`/`node`/`admin`），文件组整体挂 `requireRole(user)` ⇒ 无法表达
「可读不可写」的账号（备份/审计/只读集成场景）。目标：新增第四档 `reader`（可 GET/list/search/
stat/download，禁一切写），且现有 `user` 语义零变化。

## 定案与取舍（仍有效）

1. **角色层级 `reader < user < node < admin`**，门限判定集中在一个 `requireRole(principal, minRole)`：
   `reader` 门放 {reader,user,admin}（拒 node）、`user` 门放 {user,admin}、`node` 门放 {node,admin}、
   `admin` 门仅 admin；**未知门限 fail-closed 403**（防未来接线意外放行——这条一直有效，别改）。
2. **只读/写子组由路由分类单一事实源决定**（`ClassReader` → `requireRole(reader)`），不靠「在各
   handler 里判角色」——避免清单漂移（新增 GET 路由漏列 ⇒ reader 被误拒）。
3. **同一分类覆盖隧道内层**：外层认证放行后再按子组判（reader 读放行、写 403）；`/tunnel` 隧道面
   只验签不查角色，因此**内层是唯一收口点**。
4. **reader 不并入 `api_keys`（Bearer 多用户）**：后者是独立特性、恒 `user` 语义；两套权限模型
   纠缠会产生双轨语义（明确不做）。
5. **`admin` 不可经管理端点创建**（同一 Ring 无法新增 admin 的既有红线）；空 `role` 载入归一 `user`
   （旧 `credentials.json` 无 role 字段零迁移）。
6. **持久化形态**：`role` 是 `credentials.json` 条目上的字符串字段，`reader` 作为**显式非空值**
   不受「空值归一 user」影响 ⇒ 无迁移面。

## 落地指针

`pkg/accesskey/accesskey.go`（`RoleReader` 等常量 / `Key.Role`）、`pkg/server/auth.go`（`requireRole`）、
`pkg/server/routes.go`（`fileRouteRead` + 分类→包装器映射）、`pkg/server/routes_files.go` /
`pkg/server/route_class_index.go`（只读子组分类清单）、测试 `pkg/server/rbac_roles_test.go`。

## 实测差异（以代码为准）

- 设计「片2」计划在 `POST /api/credentials`（`akAddHandler`）增加可选 `role` 字段并允许
  `reader`/`user`；**实际实现只接受 `user`/`node`**（`admin` 拒绝，`reader` 也拒绝 400）——
  reader 档位因此**可被认证/路由层执行，但没有公开创建入口**。这是本设计最重要的落地偏移，
  已记入 `docs/api.md`；若需真正用上 reader，得先补创建入口（另开 PR）。
- 设计提的「`isReadOnlyFileRoute` 前缀组 + `localMuxGate` 拆分」在实现中收敛为**路由表分类**
  （`router.ClassReader` + 静态索引），语义等价、形态不同——不要按设计里的函数名去找。
