<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# IP 白名单 / 信任代理（`auth.allow_ips` + `auth.trusted_proxies`，11.5-②）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ip-whitelist.md` 已于 2026-11-13 归档。
> 配置键与生效范围以 `docs/config.md`（§服务端配置 · `auth.allow_ips` / `auth.trusted_proxies`）为准
> （现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

认证入口（`authMiddleware`）此前**无任何来源 IP 门**，只有 per-IP 限流键（`normalizeRemoteIP`）。
目标：认证**前**的来源 IP 白名单 + 反向代理场景下的真实客户端 IP 解析，且双配置为空时**零回归**。

## 定案与取舍（仍有效）

1. **门放在认证最前**：白名单未命中 → 403（统一文案），不泄露认证面；公开凭据端点
   （register/nonce/login）与 OIDC/LDAP 登录面同样挂门——白名单部署要求所有公开入口一致。
2. **探活/静态资源不门**：`/livez`、`/readyz`、`/healthz`、`/version`、`/metrics`、`/ui/` 保持可达
   （否则白名单误配连排障都做不了）。
3. **XFF 只在信任代理解析**：未配 `auth.trusted_proxies` 时**一律忽略** XFF（防伪造）；命中信任
   代理才从 XFF 链右向左取首个非信任项（标准语义）。畸形/超长/全信任 → 回落 `RemoteAddr`
   （fail-closed）。反向代理部署必须同时配 `trusted_proxies`，否则白名单按直连 IP 判、代理来源恒 403。
4. **回环不自动放行**（有意取舍）：白名单必须显式含 `127.0.0.1`/`::1`；为避免运维自锁，启动时对
   「已启用但未含回环」打印响亮警告——不提供绕过通道，保持单一路径语义。
5. **IP 解析单点复用**：同一解析结果供 IP 门与 per-IP 限流桶键（含 nonce 池 IP 绑定）使用，
   避免「门按一个 IP、限流按另一个 IP」的双口径。
6. **fail-closed 配置校验**：非法 CIDR 拒绝启动（不静默忽略）；纯 IP 按 `/32`、`/128` 归一。

## 外部契约

- 403 文案固定为 `forbidden: ip not allowed`；拒绝**不落审计**（非业务事件，防日志洪泛），只 Warn。
- 风险（如实披露）：启用 `trusted_proxies` 后 per-IP 桶按真实客户端 IP 计量——多用户共享同一出口
  IP 时桶会合并（特性启用后的预期行为）。

## 落地指针

`pkg/server/client_ip.go`（`resolveClientIP` / `clientIPFromRequest` / `ipGate`）、
`pkg/server/auth.go`（`authMiddleware` 最前的 IP 门 `ipGateAllowed`）、
`pkg/server/routes.go`（`ClassPublic` + guard 的包装链追加 `ipGate`）、
`pkg/server/config/{config,validate,load}.go`（字段 / 校验 / 回环警告）、
`pkg/netutil/ip.go`（`ParseIPNets` / `AllowIPsCoverLoopback`）、
`pkg/server/ratelimit`（`SetClientIPFn` 注入桶键解析）。

## 实测差异（以代码为准）

- 设计写 `docs/config.md` / `docs/cli.md` 文档同步属「片2」；实际补的是 `docs/config.md`（本任务），
  两配置**没有** CLI 面（服务端配置项），`docs/cli.md` 无对应内容。
- 设计未提及的一点（实现补齐）：`/s3/` 自带 SigV4 且不经 `authMiddleware`，因此**不受** `allow_ips`
  门约束——白名单不能当作 S3 面的来源限制（映射到部署口径时注意）。
