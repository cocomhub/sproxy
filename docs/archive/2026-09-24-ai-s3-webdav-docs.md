<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3/WebDAV 直连接入文档（11.9-②）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-s3-webdav-docs.md` 已于 2026-11-13 归档。
> 现行文档为 `docs/ai-integration.md` §2（S3 `/s3/` + WebDAV `/dav/`），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
`/s3/`（SigV4 纯标准库验签）与 `/dav/`（RFC 4918）端点早已完备，但缺面向用户/AI Agent 的接入文档
——LangChain 的 S3Loader/WebDAV loader、rclone、aws cli 无法直接上手。**非目标**：不改端点行为、
不新增 S3 网关特性。

## 定案与取舍（仍有效）
1. **纯文档 + 门禁，不改任何端点代码**（零回归）。
2. **能力边界如实列出**：不支持的项（ACL/分页/版本/事件通知）明示，不承诺 roadmap 未落地功能。
3. **凭据安全**：文档只教「如何配」，示例一律用占位符（`<AK>`/`<SK-HEX>`），绝不出现真实密钥。
4. **硬约束要写明**：region 必须 `us-east-1`、必须 path-style（非 virtual-host）——这是自研 SigV4
   与外部工具互通的硬条件。

## 与设计稿的差异（避免误读）
- 设计稿规划的文档路径 `docs/integrations/s3-webdav.md` **未采用**：接入内容最终并入
  `docs/ai-integration.md` §2。
- 设计稿的可选 `POST /api/integrations/self-test` 连通性自检端点**未实现**（可选片，从未接线）。

## 落地指针
`docs/ai-integration.md` §2（S3/WebDAV 接入 + LangChain/aws cli/curl 示例）；端点实现
`pkg/server/s3`（`s3.New(Deps)`/`Service.Handler`）、`pkg/server/dav`（`dav.New(Deps)`/`Service.Handler`）。
