<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SLO 指标（p99 延迟直方图 + Apdex）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-slo-metrics.md` 已于 2026-11-14 归档。
> 指标与查询见 `docs/api.md` §SLO 指标；配置键见 `docs/config.md`（`apdex_threshold`）。
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
Metrics 已有 40+ 指标但**无延迟分位数**——无法回答「p99 延迟多少」「SLO 是否达标」。
目标：请求处理时长直方图（支撑 `histogram_quantile(0.99, ...)`）+ Apdex 评分（阈值可配）。

## 定案与取舍（仍有效）
1. **标准库手写、无 prometheus client 依赖**：延续 `/metrics` 手写文本格式。请求延迟是**每请求
   高频**事件 ⇒ 直方图必须**无锁**（bucket 数组 + `atomic.Int64`，不用互斥），与 `RequestsTotal`
   同级开销（刻意不用互斥保护计数）。
2. **零回归靠「只加不改」**：`RecordRequest` 签名不变、middleware 仅加 `time.Now()`/`defer`、
   `/metrics` 输出追加在**尾部**（既有行逐字不变）。
3. **Apdex 阈值 T 走启动装配**（`SetApdexThreshold`），`<=0` 忽略并 Warn（禁静默降级）；
   代价是 **SIGHUP 不热载**（重启生效）——文档已注明。
4. **无样本显式可发现**：无样本仍输出 `HELP`/`TYPE`；`/metrics` 自身请求也计入（简单一致，
   排除 `/metrics` 留后续片）。
5. **命名唯一性**：`sproxy_request_duration_seconds_*` / `sproxy_apdex*` 全仓唯一（防与既有指标冲突）。

## 外部契约 / 踩坑
- bucket 为**累计**计数（非增量）、`le` 升序、`+Inf` 恒在末位、`*_duration_seconds` 命名（Prometheus
  约定）——面板/告警依赖这些形态，不可改。
- Apdex 分档边界固定：`d <= T` / `T < d <= 4T` / `d > 4T`；`score = (satisfied + tolerated/2) / total`，
  `total=0 → 0`（不 NaN）。
- 渲染并发漂移（多次 `atomic.Load` 间计数轻微不一致）与 Prometheus client 同语义，可接受。

## 落地指针
- `pkg/server/metrics/metrics.go`：`durationHistogram` / `NewDurationHistogram` / `WriteHistogram` /
  `RecordRequestLatency` / `SetApdexThreshold` / `defaultApdexThreshold`（`sproxy_request_duration_seconds`、
  `sproxy_apdex` + 三档 counter）。
- `pkg/server/config/config.go`：`ApdexThreshold`（`apdex_threshold`）。
- 测试：`pkg/server/metrics/{histogram,apdex,render}_test.go`、`pkg/server/slo_metrics_test.go`。
