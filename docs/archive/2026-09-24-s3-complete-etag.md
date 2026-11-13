<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3 CompleteMultipartUpload：ETag 校验 + 会话归属 — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-s3-complete-etag.md` 已于 2026-11-13 归档。
> 端点语义与状态码以 `docs/api.md`（§协议挂载面 ·「S3 分块上传语义（Multipart）」）为准
> （现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

`complete` 此前**不读 init 写的会话 meta**（可拿别人的 key 完成自己的上传）、**完全不校验
客户端回传的 `ETag`**（可提交任意/伪造 ETag，与落盘 part 内容无关）、请求体无上限（OOM DoS）、
失败路径残留半截目标文件。目标：按 S3 协议补齐**会话归属、part 内容校验与失败不变量**。

## 定案与取舍（仍有效）

1. **会话归属用 init 落盘的 meta 判**：meta 缺失或内容 ≠ 当前 key ⇒ `409`（统一「会话无效或
   key 不匹配」文案，不区分「不存在」与「key 不匹配」——不给探测面）。
2. **ETag 用「去引号后 == 落盘 part 内容 MD5」判**：单遍拷贝 + 哈希（`io.MultiWriter`，零额外
   IO）；客户端带/不带引号都接受（归一化）。这是**行为变更**（此前接受任意 ETag），属协议合规修复。
3. **失败不变量：宁可不留痕**——任何 `400`/`500` 返回前，若目标已打开就必须关闭 + best-effort 删除
   （含其旧 sidecar，否则旧 sidecar 变孤儿），但 **parts 保留**（允许客户端重试 complete）。
4. **错误码语义修正**：part 引用不存在从 `500` 改 `400`（这是客户端引用了不存在的东西，不是服务端故障）。
5. **输入合法性前置**：`PartNumber ∈ [1, 10000]`、拒绝重复、body 有界（`413`）——都在排序/落盘**之前**判。
6. **响应加复合 ETag**（`hex(md5(concat(md5(p1), …)))-N`）：S3 分块标准形态，加法变更（标准客户端
   忽略未知元素），由人工决策确认为「要加」。
7. **明确残余**：complete 与 abort/并发 complete **无会话级锁**（沿用现状，本片不引入锁）。

## 外部契约

- 状态码：`409` 会话 meta 缺失或 key 不匹配 / `400` 无 part、`PartNumber` 非法或重复、part 不存在、
  ETag 不匹配 / `413` body 超限 / `500` 目标创建失败或拼接 IO 错误。
- 单 part 响应 `ETag` = 该 part 内容的 MD5（带引号）——complete 侧校验依赖这个契约。

## 落地指针

`pkg/server/s3/multipart.go`（`s3CompleteMultipart` + `s3CompletePrepare` / `s3CompleteReadBody` /
`ValidateCompleteParts` / `ReadMultipartMeta` / `NormalizeETag` / `s3CompleteWriteParts` /
`removePartialS3`）、测试 `pkg/server/s3_complete_hardening_test.go` 与 `pkg/server/s3/multipart_test.go`。

## 实测差异（以代码为准）

- 设计里的「目标文件写入」补充了一条实现约束（设计未写）：目标写走**用户可见入口**（透明加密卷
  必须写密文），这是后续加固时补的，引用设计时别按「裸 `OpenFile`」理解。
- 设计的「可选增强：响应 ETag」已确认落地为**默认行为**（不是可选项）。
- 失败清理不仅删半截目标，**还删其旧 sidecar**（比设计写的更严），防止陈旧 sidecar 造成读校验
  固化失败与 meta 桶配额虚高。
