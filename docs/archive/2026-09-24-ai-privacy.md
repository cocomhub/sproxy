<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# AI 数据隐私（11.9-⑧）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-privacy.md` 已于 2026-11-13 归档。
> HTTP 契约以 `docs/api.md`（`GET /api/ai/privacy` / `POST /api/ai/privacy/purge`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
文件本体已由 at-rest 加密卷保护，但索引快照与审计日志是明文 JSON。AI 派生数据（向量 embedding、
摘要/标签文本、抽样文本摘录）若照抄明文模式会泄露内容指纹。目标：AI 派生数据一律**复用加密卷**
落盘 + 外发最小化 + 用户可见性/删除权。

## 定案与取舍（仍有效）
1. **一律经加密卷 Root 读写**，禁止绕过 Root 裸写（`os.WriteFile`）。**卷未加密 ⇒ 装配期拒绝启用
   AI**（fail-closed，宁可不启用也不落明文）。
2. **外发最小化**：embedding/摘要只发送首 4KiB 抽样；**不**整文件外发；prompt 不拼接任意用户注入
   内容（user 消息仅含文件名 + 抽样）。
3. **审计脱敏（关键）**：`ai.*` 事件 detail 只记 rel/耗时/估算 tokens，**不含摘要文本/标签正文**
   ——防审计日志成为明文内容泄露面。
4. **purge = 用户行使删除权**（非自动 GC）。范围明示：向量/摘要/标签；**审计日志不在清除范围**
   （审计是合规留存，单独说明）。
5. **密钥 = 卷主密钥**：不引入新密钥、不落日志/配置明文；密钥轮换随卷既有机制。

## 落地指针
`pkg/server/ai/privacy.go`（`AIPrivacy`：`Store/Load/Delete/List/PurgeOwner`）、路由
`GET /api/ai/privacy`、`POST /api/ai/privacy/purge`；配置 `notify.ai_privacy`；审计事件
`ai.privacy_purge`。
