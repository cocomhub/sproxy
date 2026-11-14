<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一转存/归档 + 分块打包封装 — 归档

> **归档说明**：原文 `docs/designs/2026-10-04-unified-transfer-archive.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/architecture.md` §统一转存（归档 = 转存 + 预处理）与 `docs/api.md`
> （`target_volume`/`target_path`/`encrypt`/`cipher`/`compression`）、`docs/cli.md`。本文件只保留
> **核心结论 / 抽象边界 / 未做方向 / 落地指针**。

## 1. 核心结论（用户裁定）
**归档 = 转存 + 可选预处理。** 两者底层都是「产物流(io.Reader) → 目标 writer」；**打包是预处理行为**
（与写到哪无关），**落盘是转存行为**（与打不打包无关）。拆分比「特化 archive handler」更合理。

```
统一「转存」管道： 源(source) ─→ [可选预处理] ─→ 转存(destination 目标卷)
                        │                    │
                  读/解密         None(单文件原样) / Pack(tar.gz|zstd|brotli) / Pack+Encrypt(tar.aes)
                        └──► 可选解密（source 在 secretdata 卷）◄──┘
```

行为正交：无预处理 = 纯转存（现状 transfer 语义）；有预处理 = 打包后转存到任意卷。

## 2. 抽象边界（不变量）
- **单一 code path**：`读 source → [预处理] → 写 destination`，复用 `transferFS`/`WriteIfAbsent`/ACL/
  配额/失败队列/secretdata 加密。
- 预处理层可插拔（注册类似 volume/registry）：`nil`（纯转存）/ `tar.gz|zstd|brotli`（`PackPreproc`，复用
  `compressx`）/ `tar.aes`（`Pack+EncryptPreproc`，复用 archiveCipher + compressx）。
- 落地对齐既有能力：目标卷解析 `transferFS`/`volume/registry`、原子写不覆盖 `sync.WriteIfAbsent`、
  ACL `volumeAllowedFor`/`checkTransferACL`、配额 `QuotaWriter`、加密 secretdata/shardseal、失败队列待重试。

## 3. 未做方向（记档，后续片）
- **组级打包转存**（步骤 2）：组/批量下载完成后服务端打包 → 转存单一 tar 到卷（多文件 → consolidated）。
- **图片分块打包**（步骤 3，未实现）：设想把「图片组」当作「每图 = 一个分块」，**只汇总元信息**
  （`name,size,offset,mtime,分块索引` → 档案 meta）、**不真实读写数据**（分块规划 + 元数据先落），
  随机访问单图 = 按 index 解密该分块。与 #733 视频关键帧 seek **同构**（`shardseal` 已有
  `BlockPolicy`/`MediaKindOf`；每文件作一个 block）。两个等价实现方向（择一）：**A. secretdata 扩展**
  「打包卷类型」（外层 pack 接口把 N 文件聚合为单 meta + 分块，挂到 secretdata 索引模型）或
  **B. 加密包提供打包封装**（`shardseal.PackImages(entries, outDir, secret, policy)` + `OpenImage(index)`）。
  本期**只登记方向 + 固化 API 界**，不实现字节层。

## 4. 落地指针
- `pkg/transfer/`（`registry.go`：`Preprocessor`/`PreprocessorFactory`/`Register`/`Lookup`/`New`；
  `transfer.go`/`cipher.go`/`readback.go`）、`pkg/server/archive`（server 作用域打包助手）、
  `pkg/compressx`（压缩）、`pkg/cloud`（转存内核）。
