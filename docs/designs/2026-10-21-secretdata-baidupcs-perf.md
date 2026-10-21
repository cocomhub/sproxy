<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secretdata-over-BaiduPCS 上传性能与启动索引优化设计（残余 3、4）

> 状态：设计（分支内）。目标：消除 secretdata over baidupcs 场景的两项已知残余——
> ③ baidupcs staging 上传的本地峰值与双重拷贝、④ secretdata 卷启动 `loadIndex`
> 对网盘递归列目录的分钟级慢。关联权威文档：`docs/secret-volume.md` §9.1（已登记残余）。

## 0. TL;DR

- **残余 3（上传性能/磁盘）**：现状块级交错 + staging 路径复用已把「每文件 2×」降为
  O(1 块)，但 baidupcs `Storage.Put` 内部仍有三处**整文件级**的重复计算/落盘（md5 预扫 +
  分块 md5 列表读回 + 二进制 upload 的子进程整文件流），且大文件上传后必走一次 rapidupload
  秒传刷新（ETag 非权威）。优化方案分两档：
  - **P1（本设计可落地）**：`Storage.Put` 增加「先 Stat 秒传命中检查 + 单遍 md5」前置，
    命中即零上传；给 secretdata 的交错路径加「块级 md5 增量」（每块上传时顺带累计整文件
    md5/分块 md5 列表，省去上传后 `blockMD5ListOf` 整文件读回）；
  - **P2（后续片）**：库路径（`libraryAdapter.Upload`）从「整文件 staging → 分片」改为
    **流式直传**（secretdata 单块密文 → 网盘分片直接上传，跳过本地整文件 staging），
    本地峰值从 O(块数×单块) 降到 O(1 块) 且省一次整文件拷贝——需百度分片 API 支持
    流式 TmpFile（上游 `MultiUploader` 支持 `rio.ReaderLen64` 对内存/流源的约束待验证）。
- **残余 4（启动索引）**：`SecretdataFS.NewFS → loadIndex` 对底层递归列容器树 + 每个容器
  列目录 + 每文件 meta 解密（scrypt 派生受限并发）。网盘后端（baidupcs）每层目录一次
  CLI `ls` + 批量 `meta` ⇒ 数百目录 × RTT，WSL 实测整卷 35min 未完成。优化方案：
  - **P1：本地索引快照**（对齐 `pkg/files` 的 `index_save_interval` 模式）——`NewFS` 启动
    先读快照、后台增量/失效重建；写路径在 commit 时同步更新内存索引并定期落快照；
  - **P2：网盘后端批量列举**（`baidupcs` 增加「单次 API 递归列举卷根」替代逐目录
    `ls`/`meta`——百度 `filemanager/list` 支持 depth 递归，一个请求拿全树）；
  - **P3（安全兜底）**：快照损坏/缺失/与物理不一致 ⇒ 回退全量重建（fail-closed，不静默
    空视图）。

## 1. 现状与根因

### 1.1 残余 3：上传路径（现状）

```
HTTP 上传 → files.Service.writeExternalSettle
  ├─ 独立一遍流式读算 SHA-256（O(1) 内存，源可 seek 时）
  └─ secretdata.WriteFile（multipart.File 可 seek → 流式路径）
      └─ writeFileStream（块级交错，本地峰值 O(1 块)）
          └─ 每块：加密 → 落 tmp → uploadLocalChunk
              └─ inner=baidupcs StorageFS.WriteFile（WithStagedLocalPath 复用该本地块路径）
                  └─ Storage.PutFromStagedPath
                      ├─ stageUploadFromPath：整块文件读一遍算 md5/crc32（O(块)）
                      ├─ fillStagedLists：再读回算 sliceMD5 + 分块 md5 列表（O(块)）
                      ├─ putRetryLoop：
                      │   ├─ 每轮先 Stat（秒传命中直接返回）
                      │   ├─ adapter.Upload（二进制 CLI 或库分片）
                      │   └─ Stat 复核 ETag == 本地 md5（大文件 ETag 恒非权威 → 不匹配）
                      └─ 不匹配 → rapidupload 秒传刷新 → 再 Stat 复核
```

问题点：
1. **每次上传**（每块密文）都有「整块读一遍 md5 + 整块读回分块列表」两遍全读——对
   secretdata 的交错路径，每块都重复付这个 O(块) 代价；
2. **大文件**（>4MB）分片上传后远端 ETag 是「片组合/可能不正确」（百度 Stat 语义）⇒
   md5 复核必不匹配 ⇒ **每块都多一轮** Stat + rapidupload + Stat（3 次 API 往返）；
3. 库路径 `libraryAdapter.Upload` 仍走「整文件 staging → 分片上传」——secretdata 单块
   密文已落本地 tmp，又要拷进 baidupcs staging 再分片（中间态 2×单块，虽小但重复拷贝）。

### 1.2 残余 4：启动索引（现状）

```
NewFS → loadIndex
  ├─ ListDir("") → dirContainers 递归 walkContainers（每容器 ListDir 一次）
  │     └─ baidupcs：每目录一次 CLI ls（+ 文件条目批量 meta）
  ├─ scanAllContainerDirMetas（每容器读目录 meta blob + scrypt 解密，loadGate 限并发）
  └─ loadAllFileMetas（每文件读 meta blob + 解密）
```

- 本地卷：全在本地磁盘，毫秒-秒级；
- baidupcs 网盘卷：**每层目录 = 一次 CLI `ls` 子进程 + 批量 `meta`**，数百目录 ×
  (进程启动 + RTT) ⇒ WSL 实测整卷 35min 未完成（e2e 注释已记录）。卷越大越慢，重启
  必付（无缓存）。

## 2. 方案

### 2.1 残余 3：上传性能（P1 + P2）

**P1（本设计落地，改动小）**：

1. `Storage.PutFromStagedPath` 前置**秒传命中检查**：`putCheckExisting` 已在每轮首做，
   但 secretdata 交错路径每个块都新建 → 命中率极低。改为**不变量**：仅在整文件首次
   上传前做一次 Stat（命中即零上传）；之后各块无意义（块名含随机 salt，永不命中）。
   ⇒ 省每次上传的首 Stat（1 次 API 往返/块）。
2. **块级 md5 增量**：secretdata `interleavedChunkSink` 每块加密后已有块内容，上传时把
   该块字节**同时喂给** md5/crc32/分块 md5 累计器（新增可选 `storage.PutFromStagedPath`
   的 `StageHints` 参数或在 ctx 传递「已算好 md5」的块描述）——省 `stageUploadFromPath`
   的整块重读（1 遍 O(块)）+ `fillStagedLists` 的读回（1 遍 O(块)）。
   接口形状：`PutFromStagedPath` 增加可选 `PutHints{Md5, SliceMD5, Crc32, BlockMD5s, Size}`
   参数（`md5`/分块列表为空时按现状回退自算——向后兼容，其它调用方零改动）。
3. **非权威 ETag 收敛**：`checkAfterUploadUnreliableMD5` 已在刷新后按 size 兜底。对
   secretdata 的**单块密文**（随机内容、无意义 md5）可进一步：ETag 非权威时直接按
   **size 一致**接受（跳过 rapidupload 刷新）——单块是新生成的随机密文，秒传命中率
   理论为 0，刷新纯属浪费。需保留「小文件单传 ETag=权威」的既有断言路径（改动限定
   于「块内容随机、无复用价值」的场景，经 `PutHints.RandomBlob` 标记由 secretdata
   设置）。

**P2（后续片）**：

4. **流式直传**：`libraryAdapter.Upload` 增加「staging 路径 → 分片直传」变体——调用方
   已持有**单块密文文件**（secretdata tmp），上传器直接把该文件按 4MiB 分片逐片传
   网盘（`MultiUploader` 的 `rio.FileReaderAtLen64` 可 seek，无需先整拷进 staging）。
   省：baidupcs staging 的第二份拷贝（secretdata 块 → baidupcs stage/）。
   前置验证：上游 `MultiUploader` 分片对 `*os.File` 直读（不要求先落 staging 目录）——
   现状 `putRetryLoop` 的 md5/断点/秒传参数都要整文件读回，流式直传需按片累计。

**验收（P1）**：
- 单元：`Storage.PutFromStagedPath` 带 `PutHints` 时不重读块文件（fake ReaderAt 断言
  读次数）；`checkAfterUploadUnreliableMD5` 对 `RandomBlob` 走 size 接受不调 rapidupload
  （fake adapter 计数 RapidUpload 调用）；
- e2e：真实网盘 `TestE2E_SecretdataOverBaidupcs` 保持绿（上传后内容一致 + 远端只有密文）。

### 2.2 残余 4：启动索引（P1 + P2）

**P1：索引快照（对齐 `pkg/files` index_save_interval 既有模式）**

- `SecretdataFS` 新增快照落盘：`<卷根>/.snapshot/secretdata-index.json`（卷根内隐藏文件，
  不进用户视图；baidupcs 卷根即 `extra.root` 子目录）。
- 快照内容：`{layout_version, volVersion, dirSegs, dirIDs, dirParents, dirMTime, index:
  {rel: {metaName, dirSeg, dataDir, size, mtime, phys, meta(BlockletMode/Failures 等)}}}`
  ——足够 `NewFS` 直接重建内存索引，**无需**递归列容器 + 读每文件 meta blob 解密。
- 生命周期：
  - `NewFS` 启动：先读快照（无/损坏 → 全量重建，fail-closed 不静默空视图）；
    有 → 直接装内存索引，**后台**启动增量校验（可选：不校验 = 快照即权威，靠写路径
    维护；校验 = 遍历容器树比对，若物理漂移则重建——防快照与磁盘不一致的静默错）。
  - 写路径：`commitEntry` 更新内存索引（现状已做）+ **定期落快照**（
    `snapshot_interval` 缺省 60s，对齐 `index_save_interval` 模式；关闭 = 0）。
  - 删除/改名/目录操作同样更新快照（复用内存索引即单一写点）。
- 配置：`extra.snapshot_interval`（0 = 关闭快照，启动仍全量重建——兼容现状）。

**P2：网盘批量列举**

- `baidupcs` 增加「单次 API 递归列举」：百度 `filemanager/list`（库 `FilesDirectoriesList`
  已有单层；递归变体 `depth=∞` 需 fork 库扩展或 CLI `tree` 模式）——一个请求拿全树
  （目录 + 文件 + size + mtime），替代逐目录 CLI `ls`+`meta`。启动 `loadIndex` 只发
  1-2 个请求。若 API 不支持递归，退化为「并行逐目录列举」（并发 4-8 替代串行 CLI 子
  进程）。

**验收（P1）**：
- 单元：写 N 文件 → 快照落盘 → 新 `NewFS`（mock 底层）启动**不调用** ListDir/OpenRead
  （断言底层 FS 零读）即装出完整索引；物理删除快照文件 → 回落全量重建（调用列表非空）；
- 一致性：快照载入索引与全量重建索引对同一卷**逐条目相等**（size/mtime/meta/phys）。

## 3. 影响面与边界

- **残余 3 P1**：`StorageAPI` 接口新增可选能力（`PutFromStagedPath` 加 hints 参数——
  独立 module 内接口扩展，`baidupcs` 单 module 无跨 module 影响；`secretdata` 通过
  `WithStagedLocalPath` ctx 补充或新可选接口传递）。
- **残余 4 P1**：`SecretdataFS` 新增快照字段与落盘逻辑；**多进程共享卷**（远程/多进程
  GC 场景）快照过期风险——由「写路径单调递增 volVersion + 启动校验/重建」兜底（对齐
  本地卷 `index_save_interval` 既有取舍）。
- **不改磁盘格式**：快照是**辅助索引**，任何时刻可删；物理布局 / meta / 块命名零变化，
  已落盘卷零迁移。

## 4. 决策点（待确认）

1. 残余 3 P1 的 `RandomBlob` 标记：secretdata 单块密文是否值得「跳过 rapidupload 刷新」
   ——需要运维对「内容可能被复用」场景的默认行为裁决（当前所有块都含随机 salt，理论
   命中率为 0；保守默认可**不做**，仅做 ①②）。
2. 残余 4 P1 快照的**启动校验**默认开/关：开（遍历比对，物理漂移即重建）保正确性但
   启动仍慢；关（快照即权威）启动快但依赖写路径无 bug。对齐本地卷既有 `index_save`
   模式（默认关校验，写路径维护）。
3. `snapshot_interval` 默认值（60s？）与是否进 `docs/config.md` 文档化。

## 5. PR 范围（分片建议）

- 片 A（残余 3 P1①②）：`PutHints` + secretdata 块级 md5 增量 —— 独立 PR；
- 片 B（残余 4 P1）：索引快照 + 启动快速装载 —— 独立 PR（含快照/重建一致性测试）；
- 片 C（P2 项 4、5）：流式直传 + 网盘递归列举 —— 后续片（需上游 fork 库扩展验证）。
