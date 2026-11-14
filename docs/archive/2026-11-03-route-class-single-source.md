<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 路由分类单一事实源（T4 阶段三）— 归档

> **归档说明**：原文 `docs/designs/2026-11-03-route-class-single-source.md` 已于 2026-11-14 归档。
> **B1–B4 全部落地**（文件域 45 条进表 + `localMuxGate`/`fileRoute`/写面改查索引 + 删除旧谓词 +
> 分类双向锁）。现行事实源见 `docs/architecture.md`「路由表数据化与下放（`pkg/server/router`）」
> 的「分类查表（2026-11-03）」段与 `docs/gates.md` 的 **R38**。本文件只保留**问题证据 /
> 方案与被否决方案 / 实施踩坑（仍有效）/ Guard 机制**。

## 问题（实测）
装配层一条**文件域路由**最多在 **5 处**声明/分类：`srvMux`（`fileRoute`/`fileRouteRead`）、
`localMux` 裸注册、`isFileGroupedRoute` 白名单、`isReadOnlyFileRoute` 白名单、`route_classes.tsv`。
其中 **近一半的分类事实是 silent-fail 的**：`localMuxGate` 内层漏列 ⇒ **静默越权**（fail-open）；
`isReadOnlyFileRoute` 漏列 ⇒ reader 越权写 / user 被拒读（silent，权限错配）。作者已知风险但用
「列表 + 测试兜底」缓解，而不是消除。

## 方案（已落地）：分类声明一次，两侧行为派生
`router.Route[H]{Method, Path, Server Class, Local Class, H}` + `router.Register(...)`（两侧分别按类
取包装器，未知类 **panic** fail-closed）。文件域路由迁移后**只有一处声明**；`localMuxGate` 改为查
**注册期构建的 class map**（`"METHOD PATH" → Class`），不再是字符串白名单；三个谓词
（`isFileGroupedRoute`/`isReadOnlyFileRoute`/`isWriteFaceRoute`）在全部路由进表后**删除**。
**R38 新增双向锁**：生产侧表声明的 `Server`/`Local` class 与审计侧 `route_classes.tsv` 行
**逐条相等**（改分类必须同时改两处 ⇒ diff 可见）。

## 被否决方案：「由子包自行注册 handler」（结论：不做）
三层理由：① handler 需要装配态（actor/审计/配额/写面门/`deps.go` 的 12 组依赖），子包持有 handler
要么持一大票能力接口（P1-c 已证明 40 个方法才够 cloudtasks），要么反向 import 装配层（R6 禁止）；
② 安全决策必须留装配层（class→wrapper 是「谁能访问」的裁决，分散到 20 个子包会把 deny-by-default
审计面碎成 20 份）；③ **真正的重复不是「handler 住哪」，而是「同一分类事实写 5 遍」**，数据表方案
消除后者且零跨包耦合。**表跟着 handler 走，而不是反过来**。

## 实施踩坑（B4 实测，仍有效）
1. **`r.Pattern` 在包住 mux 的中间件里是空的**——模式由 ServeMux 在调用 handler 时才写入；中间件层
   必须用 `mux.Handler(r)`（只匹配、不执行）拿同一个模式串，否则查表永远 miss，表现为「文件域全部
   放行」（首次实现即踩到）。
2. **索引必须包含子包声明的文件域路由**（`share.Routes()`），否则 `fileRoute` 自检把 `/api/shares`
   判成「不在文件组」⇒ 500。
3. **表驱动比旧前缀匹配更精确**：旧 `isReadOnlyFileRoute` 用 `HasPrefix("/api/shares/")` 把不存在的
   `GET /api/shares/{token}` 也判为只读；新索引用模式串，不存在即不命中。
4. **写面声明化**：`isWriteFaceRoute` 的第二份路径清单删除，改查 `mutatingPatterns` 索引
   （文件域/分享由 `ClassUser` 派生，其余域显式 `Mutating: true`）；新增
   `TestWriteFace_EveryRegisteredRouteIsDeclared`（漏声明即红）。

## Guard 机制（credentials 域引入）
分类（Class）只表达**基础鉴权**，不表达「同一分类内的额外入口控制」。三个公开凭据入口
（register/nonce/login）各挂独立限流器 + 主 mux IP 门，引入 `Route.Guard` 维度：
`Class = 谁能访问，Guard = 额外入口控制`。Guard 记号在装配层唯一映射（`guardWrapper`，未知记号
**panic**、限流器未初始化也 **panic**）。
**变异暴露的盲区（已补断言）**：删掉 login 的 `Guard` 后，分类仍 public、R38 仍绿、全部凭据测试仍
通过——即「公开端点裸奔」无人发现 ⇒ 新增 `TestCredentialRoutes_PublicEntriesCarryGuards` 锁住三个
公开入口的 Guard。**顺带修掉的 P1**：三条公开入口在 localMux 注册时引用**尚未初始化**的限流器字段
（`nil` 方法值 ⇒ 隧道模式请求 panic 断连）；表化把「初始化 → 注册」顺序固定。

## 验收（已达成）
`isFileGroupedRoute|isReadOnlyFileRoute|isWriteFaceRoute` 全仓 0；一条文件域路由只在表一处声明；
R38 双向锁；变异：改一条路由的 class ⇒ RBAC/审计测试红；既有行为测试断言零修改。

## 落地指针
`pkg/server/router/route.go`、`pkg/server/routes_files.go`（文件域表）、`pkg/server/route_class_index.go`
（模式串 → Class）、`docs/gates.md` R38。
