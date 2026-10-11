<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 架构级重启恢复（统一任务恢复协调器）— 待办设计

> 状态：**待办（未实现）**。用户 2026-10-10 裁定：记录待办，后续单独 PR 处理，计划
> 统一恢复协调器，避免到处独立实现导致复杂性维护困难。本文件是设计背景与方向备忘，
> 供后续实现参考；不是现行实现事实源。

## 背景（为什么是架构级）

可信卷（feat/trusted-volume）演进中暴露：进程崩溃后的**处理任务恢复**（上传、下载等）
目前由各管理器/卷**各自独立实现**，无统一协调：

| 恢复点 | 现状 | 位置 |
|---|---|---|
| 云下载任务 | `recoverTasks` 重启 `downloading` → Range 增量续传 `.partial` | `pkg/cloud/manager_persist.go:71` |
| 同步任务 | `recoverTasks` 重启 `syncing/retrying` → 幂等重 diff | `pkg/syncmgr/manager.go:1310` |
| 分块上传会话 | `recoverSessions` 恢复在途 chunk 会话 | `pkg/files/chunked_store.go:1174` |
| baidupcs 上传断点 | **未接线**（`globalLayout` 恒 nil → resumeKey 空） | `pkg/volume/ext/baidupcs/resume_upload.go:24` |
| 下载断点 | `.partial` + `.partial.etag`（下载器自理） | `pkg/downloader/http_downloader.go` |

装配层无一次性的残留任务扫描 / 统一恢复编排——三处恢复各自构造函数内部完成、各扫各的
`meta/*` 目录，无共享"任务注册中心"。

## 用户裁定（2026-10-10）

- **重启恢复是架构级能力**（上传、下载等处理任务）；所有卷只负责提供**基本能力**，
  由**架构协调**处理；
- 启动时发现残留任务需**继续处理**（非丢弃）；
- 经 **meta 检查文件块正确性**，**修正异常块后继续**任务；
- 上传恢复时 meta 校验对象 = **staging 本地 meta**（本地 staging 文件 ← FileMeta 逐块校验）；
- 计划**统一恢复协调器**，避免到处独立实现导致复杂性维护困难——后续单独 PR 实现。

## 设计方向（草案）

### 统一任务注册中心
装配层（`cmd/sproxy` root.go setupServerCore / `pkg/server/RegisterRoutes`）启动时，经统一
协调器扫描全部残留任务（cloud / sync / chunked 上传会话 / baidupcs 上传断点 / 下载
partial），按优先级/依赖编排恢复，取代三处各自 recoverTasks。

### 上传断点恢复（staging 本地 meta 校验）
- 接线 `globalLayout`（当前恒 nil——`newLibraryAdapter` 注入 `layout: globalLayout`，
  仓库内无赋值点）→ resumeKey 生效 → 分块进度中途落盘（消费 fork 库
  `updateInstanceStateChan`，上传中断时每块完成即持久化）；
- 修复断点生命周期三处代码-注释不符（resume_upload.go / multiupload.go）：
  - 失败/中断路径 `saveUploadResume`（当前失败分支提前 return 不写断点）；
  - 成功路径 `DeleteResume`（当前保存全量残留不删）；
- 崩溃恢复：重启时扫描 staging 残留（`stage-*` 子目录）+ resume，用 **FileMeta
  （staging 计算产物）** 对 staging 文件逐块校验（`meta.VerifyReadSeeker` / `Calculator`）：
  - 正常块 → 经 InstanceState 续传（跳过已传分片，`checksumMap` 命中跳过上传）；
  - 异常块 → 修正（重新生成该块）后继续；
- 复用原语：`meta.Provider`（trusted_fs.go:63 / secretdata）、`meta.VerifyReadSeeker`
  （verify.go:28，错误含分块索引 verify.go:121）、`Calculator`（calculator.go:25）、
  `meta.MaxMetaSidecarBytes`（filemeta.go:266）。

### 下载续传前校验
`.partial` 续传前经 meta 逐块校验已存在字节（`hashExistingPartial` 现只做整文件哈希），
异常块回退全量重下；正常块继续 Range。

## 已验证的现状（探索结论，供实现参考）

- **上传断点功能生产下未接线**：`globalLayout` 无赋值 → `a.layout != nil` 恒 false →
  resumeKey 空 → `uploadViaMultiUploader` 恒空状态不持久化（resume_upload.go:24 /
  adapter.go:291-293 / multiupload.go:158）。
- **InstanceState**（fork 库 requester/uploader/instance_state.go:16-19）：
  `BlockList []*BlockState{ID, Range, CheckSum}` + `Uploadid`；CheckSum 是分片上传后返回的
  分片 md5，"跳过已传分片"语义正确——前提是状态能中途落盘（当前不落盘）。
- **staging 残留**（storage.go:335-340）：`MkdirTemp("stage-*")` 独立子目录 + 精确 basename；
  Put 成功 `RemoveAll` 删子目录；崩溃残留 = 不可复用整文件副本（Put 重启会重新
  stageUpload 重传）。**注意**：`StorageFS.WriteFile`（syncfs.go:163-168）是另一条并行
  staging（`staging-*` CreateTemp），与 `Storage.stageUpload` 的 `stage-*` 模式不同。
- **下载续传**（http_downloader.go）：`.partial` + `.partial.etag`，206/200/416 分派；
  续传时 `hashExistingPartial` 只算整文件哈希，不做块级校验。
- **撤销记录**：`5875a7d90`（启动清理 staging 孤儿）已 revert——用户裁定恢复应
  "继续处理"而非"丢弃清理"，且属各处独立实现方向，由统一协调器取代。

## 排期与范围裁定（2026-10-10 用户明示）

- **排期**：统一恢复协调器在**当前可信卷功能完成**且**转存功能完成**之后启动——避免在
  两处功能仍在演进时做架构级恢复编排，造成反复返工。
- **范围**：**上传断点**与**下载续传**都属于「需要协调的任务」，**一起规划、同一协调器**
  （不各自独立实现）——上传（baidupcs staging/resume）与下载（`.partial` Range 续传）
  共享同一套任务注册 + meta 逐块校验原语。
- **现状标注**：baidupcs 上传断点当前 `globalLayout` 恒 nil（不持久化，中断即重传）；
  下载 `.partial` 续传仅有整文件哈希（无块级校验）。两者均在协调器专题内一并解决。

## 后续 PR 实施清单（草案）

1. 统一任务注册中心（装配层启动编排，收敛三处 recoverTasks）
2. 接线 globalLayout + 修复断点生命周期三处不符 + 中途落盘消费 updateInstanceStateChan
3. 上传恢复：staging 本地 FileMeta 逐块校验 → 正常块续传 / 异常块修正
4. 下载恢复：partial 续传前块级校验 → 异常块重下
5. 测试：崩溃恢复 e2e（构造残留 staging+resume → 重启 → 校验续传完成）

> 启动前置：可信卷功能（本分支）完成 + 转存功能完成。
