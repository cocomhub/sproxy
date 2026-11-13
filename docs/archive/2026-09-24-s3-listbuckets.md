<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3 ListBuckets（`GET /s3/`）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-s3-listbuckets.md` 已于 2026-11-13 归档。
> 端点与用法以 `docs/api.md`（§协议挂载面 · `GET /s3/`）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
rclone / aws CLI 的标准流程是 ListBuckets → ListObjectsV2。此前 `GET /s3/` 不返回桶列表，
标准客户端无法「发现」桶。目标是补齐 `ListAllMyBucketsResult` XML。

## 定案与取舍（仍有效）
1. **桶 = 请求者 ACL 可见的「本地」卷**，口径与写路径同源（`AllowedVolumes` ∩ 有本地根）。
   **外部卷不列**：无本地根、S3 不可寻址，列出来就是广告不可用的桶。
2. **`volSet == nil` 是合法旧装配形态，不是错误** ⇒ 返回 `["default"]`，与
   `s3ListObjectsV2` 的默认桶名对齐（零回归）。
3. **明确不做 CreateBucket / DeleteBucket**：卷集合由装配期/运行时外部卷机制决定，
   S3 侧不建卷（双层命名空间问题，属产品口径而非实现缺口）。
4. **省略 `CreationDate`**：registry 无卷创建时间；aws CLI / rclone 实测容忍缺省。
   若未来需要，取卷根 mtime 兜底即可（单点函数）。
5. 卷名仍统一走 `xmlEscapeText`（纵深防御，即使值来自已校验配置）。

## 外部契约 / 踩坑
- 路由判定顺序：必须插在 `list-type==2` 分支**之后**、空 key 400 分支**之前**；
  `HEAD /s3/`（空 key）保持 200 桶存在性语义，不动。
- `/s3`（无尾斜杠）**不**命中 `/s3/` 前缀路由——既有行为，文档注明即可，不改路由。
- 该组路由分类为 `public`（mux 层不做 SproxySig），验签在 handler 内完成；
  这是既有 S3 面契约，不因本次新增而改变。

## 落地指针
`pkg/server/s3/server.go`：`s3ListBuckets`（ListBuckets handler）、`s3BucketsXML`（XML 渲染）、
`ownerBuckets`（ACL 视图 ∩ 本地根）、`s3Handler` 路由分发。
