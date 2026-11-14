<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 路由注册下放与 URL 命名规范化 — 归档

> **归档说明**：原文 `docs/designs/2026-11-02-route-group-registration.md` 已于 2026-11-14 归档。
> **组 router 内核与子包路由下放均已落地**（`pkg/server/router`，T4 机制；后续见
> `docs/archive/2026-11-03-route-class-single-source.md`）。现行事实源见 `docs/architecture.md`「路由表数据化与
> 下放（`pkg/server/router`）」与 `AGENTS.md` 能力矩阵（`pkg/server/router` 行）+ `docs/gates.md`
> 的 **R38**。本文件只保留**动机 / 核心决策与红线 / 被否决方案 / 未落地的 URL 命名提案 /
> 性价比结论**。

## 背景（2026-11-02 实测）
`RegisterRoutes` 是 **613 行单函数 + 231 处内联注册**；双 mux 重复注册 **100 条**；子包
（cloudtasks/share/notify/ai/verify）路径全部写死在装配层（新增端点必须改装配根）；鉴权分类由
包装链隐含、只能靠 `route_classes.tsv` 事后镜像（两份事实源易漂移）。

## 核心设计（已落地）
- `pkg/server/router.Group`：一组共享「前缀 + 鉴权策略」的注册面；分类在**构造期由装配层固定**，
  子包只能用、不能改；`Handle`（双端注册）/`Server`（仅外部面）/`Local`（仅隧道面）/`Sub`（前缀
  拼接）。
- 子包侧：`RegisterRoutes(g *router.Group, bind func(Op) http.HandlerFunc)`；`bind` 由装配层提供
  （内含 `ActorFrom` 等装配语义——遵循 P1-c 裁决：**ctx→actor 不进领域包**）。
- 一条事实源：**组 = 前缀 + 分类 + 包装器**；新增端点只改子包一行，装配层零改动。

## 红线：为什么不直接「子包自注册 + 自选包装器」（被否决方案）
分类决定「一条路由要不要鉴权、要不要角色门」，是**安全边界**。让每个包自选分类会：把
deny-by-default 的审计面拆成 19 份（R38 无法再靠一张表通读）；把「选错分类」从装配层一处扩散到
每包每路由；与 R38 现行判据（从 wrapper 链机械提取分类）不再兼容。⇒ **前缀与路径下放、分类留在
装配层**。

## URL 命名规范（提案，**随迁移分批，未全量落地**）
规则：① 集合用复数名词（`/api/cloud/tasks`、`/api/shares`、`/api/volumes`）；② 单例资源用单数
（`/api/config`、`/api/stats`）；③ 动作作为子资源（`POST .../tasks/{id}/cancel`），禁止新增「集合上的
裸动词」（`/api/volumes/move`、`/copy` 属历史遗留）；④ 无 `/api` 前缀的路由只允许三类（健康检查、
公网分发 `/s/{token}`、协议挂载 `/dav/` `/s3/` `/ui/` `/tunnel`）；**文件面遗留路径
（`/upload` `/download` `/delete`...）保持不动**（外部客户端依赖，改名为负收益）。
**已确认但未改的不一致**：`POST /api/share`（单数创建）vs `/api/shares/{token}`（复数）；
`POST /api/volumes/move|copy|rebalance|refresh`（裸动词）；`POST /api/notify/test`。迁移策略：
**URL 改动单独成 PR，同一 PR 内同步** `pkg/client`/`cmd/sclient`/`web/static`/`docs/api.md`/
`route_classes.tsv` 与测试；不保留旧路径 alias；破坏性变更按 `feat(cli)!` 标注。

## 性价比结论
组 router 内核 + 子包路由下放（cloudtasks 32 条）= **做**；其余子包（share/notify/ai/verify）分批；
`files`/`volumes` 路由（~90 条，handler 全在装配层）= 需先切片，**暂不做**；URL 规范化 = 单独 PR，
优先级低于注册下放；全量 URL 重命名（文件面） = **不做**。

## 门禁（R38）
扫描面从「routes.go 的 wrapper 链」改为「`router.New(...)` 构造点的 class + 各组注册的
`(method, path)`」；`route_classes.tsv` 保留为**期望表**，由测试运行期构建实际路由表后比对
（不再解析源码行号）；新增子包若忘记在装配层建组 ⇒ 表比对红。

## 落地指针
`pkg/server/router/route.go`、各子包 `routes.go`（`RoutePrefix` + `Routes()`）、装配层 `h.routeWrapper`、
`docs/gates.md` R38。
