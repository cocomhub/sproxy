<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 限流维度扩展（端点维度 + 并发闸）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ratelimit-dimensions.md` 已于 2026-11-13 归档。
> 配置字段以 `docs/config.md`（`rate_limit.endpoints` / `endpoint_default` / `max_concurrent`）
> 为准（现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
既有 `RateLimiter` 只有 per-IP 令牌桶 + 全局滑动窗口 + 跨实例协调计数。缺两类维度：
**按端点**限流（保护昂贵端点）与**全局并发上限**（防同时处理请求数失控）。

## 定案与取舍（仍有效）
1. **放行链顺序固定**：并发闸 → per-IP 桶 → per-endpoint 桶 → 全局窗口 → coordinator；
   任一拒绝即 429（回包格式与现状一致）。顺序固定是契约的一部分：并发闸在最前，被拒的请求
   **不消耗**其它维度的配额（避免「闸满把 IP 桶额度也吃掉」）。
2. **并发闸是非阻塞 select**（满即拒），成功者必须 `defer` 配对归还——否则槽泄漏会永久降容。
   热更新只对新请求生效，在途请求归还旧配额，仍不泄漏。
3. **只限显式配置的端点**：精确匹配优先 + `/` 段边界前缀最长匹配；无匹配规则直接透传。
   刻意不用「全局默认端点规则」，避免误伤 hub / mux 长连。
4. **非法规则值沿用默认化**（`limit<=0` → 5、`window<=0` → 1s），与既有 `rate_limit` 语义
   保持一致；不改成 fail-closed（改语义属破坏性变更）。
5. 端点键直接用 `r.URL.Path`，不做归一化（配置方负责写对路径），减少一层隐式变换。

## 落地指针
`pkg/server/ratelimit/ratelimit.go`：`AllowEndpoint`（per-endpoint 判定）、
`AcquireConcurrent` / `ReleaseConcurrent`（并发闸）、`UpdateConfig`（热更新同一把 mu）、
`Middleware`（放行链）。装配根经 `SetMetrics` / `SetClientIPFn` / `SetCoordinator` 注入依赖。
