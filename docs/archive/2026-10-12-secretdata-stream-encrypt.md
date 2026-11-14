<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secretdata 加密卷写路径流式化 — 归档

> **归档说明**：原文 `docs/designs/2026-10-12-secretdata-stream-encrypt.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/secret-volume.md`（§1「流式分块加密（可 seek 源）」、§8.2、§9）。本文件只保留
> **为什么能流式（论证）/ 不变量 / 踩坑 / 边界 / 落地指针**。

## 1. 目标与结论
消除 secretdata 写路径「**整文件明文驻留内存**」的 1× 文件峰值，把加密卷写入改造成**可 seek 源的流式加密**：
**磁盘格式 / meta 结构 / 块布局 / 分组盲签 / 校验全部不变**（向后兼容，读路径零改动）。

现状根因：`writeFileOpt` 先 `io.ReadAll(r)` 把整文件明文读进内存，再逐块加密。**块大小上限管的是
「单块临时密文/写盘粒度」，不决定整文件驻留**——分块只是对已驻留 `data` 做零拷贝切片。

## 2. 可流式化论证（核心，勿偏离）
现状「必须全文」的三个理由里，**两个只需要哈希值而非全文**，第三个本就要求 `io.ReaderAt`：
1. 整文件强哈希 `Meta.Original.SHA256 = Hash256(data)` ⇒ **可流式增量**（`sha256.New()` + 逐块 `Write`）。
2. 分组盲签 `GroupSig(secret, sha256(data))` ⇒ 只需收尾的 `fullSum`。
3. video-keyframe blocklet 规划 `parseOnce(data io.ReaderAt, …)` **本就是 `io.ReaderAt`** ⇒ 与流式天然兼容。
- 块规划 `Plan(origSize)` 只依赖**文件大小**，不碰内容；`encryptBlocklets` 只吃**当前块** ⇒ 逐块流式兼容。
- **分组盲签计算时机后移**（循环后），但值语义不变 ⇒ blob 中段命名不变。

## 3. 入口与适配
- shardseal 新增 `EncryptShardsReader(src io.Reader, size, …)`：内部 `io.ReaderAt`/`io.Seeker` 判定 →
  流式（逐块 `ReadAt` + 增量哈希 + 组签收尾）；否则回退 `io.ReadAll` + 现有 `encryptShards`。
- secretdata `writeFileOpt` 把 seek 判定**收在内部**：可 seek 且非 `Dedup`/`Erasure` ⇒ `writeFileStream`；
  纯流/受限模式回退现状（功能不变，仅内存不省）。
- **零调用方改动**：外部调用方保持传 `io.Reader` 不变（功能未上线，允许 API 直接变更）。

## 4. 关键踩坑（仍有效，最重要的产出）
- **不可 seek 只来自两层人为包装**：`io.TeeReader`（`writeExternalSettle` 算哈希）与 `rateLimitReader`
  （带宽限速）。**必须透传 seek**，否则 `io.Seeker` 断言恒失败、流式永不生效。
- **#763 回归（用 tee 算 checksum 会静默坏校验）**：tee 只捕获经 `Read` 的字节，而 secretdata 流式
  全程走 `ReadAt`（两遍）⇒ 摘要为空 ⇒ 校验失败。修法：**可 seek 源先独立一遍读满算 checksum 再
  `Seek(0)`**，把**原可 seek 源**下发给 sink（sink 可多遍随机读）；不可 seek 源才退 `io.TeeReader`
  （`feedChecksumRewindable` / `feedMetaRewindable`，同时用于 HTTP 上传外部卷、统一写内核外部分支、trusted）。
- **video-keyframe 的 `parseOnce` 读的是文件不是内存**（`io.ReaderAt` 扫描关键帧表）⇒ 峰值仍 O(块上限)。
  曾被误记为「keyframe 走上层 ReadAll 回退」，已被证伪（`TestEncryptShardsReader_VideoKeyframeParity` 锁定）。

## 5. 边界（记档）
- **必须可 seek**（`io.ReaderAt`）；纯流（stdin 直传）与加密同步 `io.Pipe` 预期不可 seek ⇒ 回退 `ReadAll`。
- `max_file_bytes` 从「唯一硬闸」变为「仍有用的文件级总闸」（防超大文件在 seek 判定前被读入）；
  块上限 `max` 成为真正的**单块内存护栏**。

## 6. 落地指针与量级
- `pkg/cryptox/shardseal/shardseal.go`（`EncryptShardsReader` + `EncryptShardsReaderToChunks`）、
  `pkg/volume/secretdata/secretdata.go`（`writeFileStream`/`writeFileStreamEntry`）、
  `pkg/files/write_ops.go`（`feedChecksumRewindable`）、`pkg/volume/trusted/trusted_fs.go`（`feedMetaRewindable`）。
- 内存实测（slow tag，64 MiB 文件）：流式（2 MiB 块）**10.4 MiB** vs 回退 `ReadAll` **185.9 MiB**；
  确定性守卫 `TestEncryptShardsReader_NeverReadsWholeFile`（永不整读全文）长期防回归。
