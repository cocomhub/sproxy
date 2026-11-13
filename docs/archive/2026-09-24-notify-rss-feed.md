<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 通知 RSS/Atom 订阅端点（roadmap 11.7-⑦）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-notify-rss-feed.md` 已于 2026-11-13 归档。
> 端点与配置以 `docs/api.md`（§GET /api/notify/feed）与 `docs/config.md`（`notify.feed_max` /
> `notify.feed_token`）为准（现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
通知中心已有 `NotifyCenter.History()`（最近在前的内存快照），但只能轮询 `/api/notify/history`。
目标是给用户/运维一个**只读订阅入口**（RSS 2.0 / Atom），让阅读器代为轮询，且**不动**
通知中心的内部状态与字段。

## 定案与取舍（仍有效）
1. **数据源零改动**：完全复用 `History()` 快照，不新增状态字段 ⇒ history 端点与各渠道发送零影响。
2. **噪音过滤**：排除 `debounced`/`skipped`，保留 `sent`/`failed`；再截断到 `maxN`。过滤与元数据
   由 RSS/Atom 两个渲染器共享（`feedEntries` + `feedMeta`），避免双实现漂移。
3. **token 门禁语义**：空 = 公开（与 history 端点一致，默认零回归，不擅自收紧）；非空 = 必须带
   `?token=` 或 `Authorization: Bearer`，常量时间比较，401 **不区分**「未提供/错误」（防 token 枚举）。
   feed 会暴露审计信息 ⇒ 生产**建议**配置 token，而不是默认强制。
4. **空历史仍输出合法空 feed**（阅读器兼容，不报错）；渲染失败理论上不出现，出现则 500 + Warn。
5. **GUID 唯一性**：时间戳 + Action + Object + Channel + Status 组合，保证同刻多渠道/多状态不撞。

## 外部契约 / 踩坑
- `Content-Type`：`application/rss+xml` / `application/atom+xml`（均 `charset=utf-8`）；
  时间格式 RSS `pubDate=RFC1123Z`、Atom `updated=RFC3339Nano`（改格式会打断既有阅读器）。
- `format` 只接受 `rss|atom`，其它值 400；`notify` 未启用时 400 复用既有文案（不新造错误串）。
- XML 全部经 `encoding/xml` 输出（自动转义，防注入）；`Link` 由请求 Host + scheme 拼绝对 URL
  （允许 `X-Forwarded-Proto` 覆盖）。
- 设计稿里的 handler 方法名（`(*Handlers).notifyFeedHandler`）与最终实现不同：实现落在
  `pkg/server/notify` 子包，经 route 表注册，不再挂在装配根。

## 落地指针
`pkg/server/notify/feed.go`（`feedEntries` / `renderRSS2` / `renderAtom`）、
`pkg/server/notify/routes.go`（`GET /api/notify/feed`）、`pkg/server/config/notify.go`
（`FeedMax` / `FeedToken`）、测试 `pkg/server/notify/feed_test.go` / `feed_edge_test.go`。
