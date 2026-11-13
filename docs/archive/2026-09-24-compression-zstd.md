<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 压缩算法扩展（11.10-⑨）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-compression-zstd.md` 已于 2026-11-13 归档。
> 参数与用法以 `docs/api.md`（archive 的 `compression`）与 `docs/cli.md`（`--compress`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
存档（archive 压缩）与传输缺 zstd/brotli 高压缩比选项。目标：算法可选、可观测，默认 gzip 零变化。

## 定案与取舍（仍有效）
1. **集中到单一编解码注册表**（`pkg/compressx`），**非法算法显式 error，不静默回退 gzip**
   ——「显式语义」优先于「宽容」。
2. **默认 gzip 逐字旧行为**（golden 归档测试锁定）。
3. **明确排除 xfer 密文层压缩**：AES-256-GCM 后压缩比≈1，纯白耗 CPU——**不做**（防误接）。
4. **块级 frame（非流级）**：每 chunk 独立 frame 才能与 chunked upload/download 的 `offset/size`
   随机读语义兼容；选流级会破坏随机读。
5. **依赖策略**：`klauspost/compress`（纯 Go、社区活跃），已按「优先纯 Go 实现」评审放行。

## 外部契约
`compression` 取值 `gzip`（默认）/`zstd`/`brotli`；非法值 → 400（不静默忽略）。归档元数据/响应头
标注算法（可观测）。

## 落地指针
`pkg/compressx`（`Algorithm` / `Parse` / `NewReader` / `NewWriter` / `Ext` / `ContentType`）、
`pkg/server/archive`（`Compression` 字段 → Content-Type/扩展名）。
