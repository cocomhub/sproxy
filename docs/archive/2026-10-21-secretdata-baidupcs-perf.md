<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secretdata-over-BaiduPCS 上传性能与启动索引优化 — 归档

> **归档说明**：原文 `docs/designs/2026-10-21-secretdata-baidupcs-perf.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/secret-volume.md` §9.2（上传提示 / 启动索引快照）、§3.2（`volume_state` 装载）、
> §3.2「`disable_meta_prewarm`」。本文件只保留**根因 / 已落地判据 / 红线 / 未做边界 / 落地指针**。

## 1. 两条残余与根因
- **残余 3（上传性能/本地峰值）**：secretdata 块级交错 + staging 路径复用已把「每文件 2×」降为 O(1 块)，
  但 baidupcs `Storage.Put` 内部仍有**整块级**重复计算（`stageUploadFromPath` 整块算 md5/crc32 +
  `fillStagedLists` 再读回算 sliceMD5/分块 md5 列表）；且大文件分片上传后远端 ETag 非权威
  ⇒ 每块多一轮 Stat + rapidupload + Stat。
- **残余 4（启动索引）**：`NewFS → loadIndex` 对底层递归列容器树 + 逐容器列目录 + 逐文件 meta 解密
  （scrypt 派生受限并发）。baidupcs 每层目录一次 CLI `ls` + 批量 `meta` ⇒ 数百目录 × RTT，
  **WSL 实测整卷 35min 未完成**（卷越大越慢，重启必付、无缓存）。

## 2. 已落地（P1）
- **上传提示（`pkg/sync.UploadHints`，跨后端中性通道）**：写面经 ctx 传「已算好的
  md5/sha256/size/…」，后端按协议 adapter 映射（baidupcs → sliceMD5/crc32/blockMD5s）；secretdata 交错
  路径每块加密后**顺带累计**，省去整块重读 + 读回两遍 O(块)。`Merge` 只补空字段（源头值优先）⇒
  「外层未知 ⇒ 内层首次算一次、之后全程复用」。**未知后端安全忽略** ⇒ 零回归。
- **secretdata 装配期索引快照**：`NewFS` 启动先读加密快照直接装内存索引，
  **损坏 / 版本不匹配 / 与物理不一致 ⇒ 回退全量重建**（fail-closed，不静默空视图）。
  写路径 commit 时更新内存索引 + 异步落快照；**任何写操作后置位抑制标记**禁止异步回写陈旧快照。
  **不改磁盘格式**（辅助索引，任何时刻可删，物理布局/meta/块命名零变化，已落盘卷零迁移）。

## 3. 红线 / 关键裁定（仍有效）
- **`RandomContent` 只跳 pre-Stat，绝不跳上传后 md5 刷新/复核**：块列表必算、秒传刷新恒做——
  用户裁定「文件一致性硬保证，任何内容形态都不得绕过」。
  （设计过程曾一度想对 secretdata 单块密文跳过 rapidupload 刷新，已否决；回归用例
  `TestStorage_PutFromStagedPath_RandomBlobStillRefreshesMD5` 钉住。）
- **快照启动校验的取舍（对齐本地卷既有模式）**：快照即权威 + 写路径维护（默认）；「遍历比对、物理漂移
  即重建」的强校验保正确性但启动仍慢 ⇒ 不做默认开启。

## 4. 未做（后续片，边界记档）
- **baidupcs 库路径「流式直传」**：staging 单块密文 → 网盘分片直接上传（跳过本地整文件 staging），
  本地峰值 O(块数×单块) → O(1 块) 且省一次整文件拷贝。**前置**：上游 `MultiUploader` 支持流源分片
  直读（`*os.File` / `io.ReaderAt`，不要求先落 staging）待验证。
- **网盘后端批量列举**：单次 API 递归列举卷根替代逐目录 `ls`+`meta`（百度 `filemanager/list` 支持
  depth 递归）；若 API 不支持则退化为并行逐目录列举（并发 4-8）。

## 5. 落地指针
- `pkg/sync/uploadhint.go`（`UploadHints`/`Merge`/`WithUploadHints`/`UploadHintsFrom`）、
  `pkg/volume/ext/baidupcs/syncfs.go`（映射）/`uploadcore*.go`、
  `pkg/volume/secretdata/index_snapshot.go`（`loadIndexSnapshot`/落盘/抑制）、
  `cmd/sproxy/secret_register.go`（`IndexSnapshotPath` 注入）。
