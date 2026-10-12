<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secretdata 加密卷写路径流式化设计（Streaming Encryption for Secretdata Write Path）

> 状态：设计（分支内）。目标：消除 secretdata 写路径「整文件明文驻留内存」的 1× 文件峰值，
> 在不改变磁盘格式 / meta 结构 / 读路径的前提下，把加密卷写入改造成**可 seek 源的流式加密**。
> 关联权威文档：`docs/secret-volume.md` §8.2（流式加密为未实现项）、§9（max_file_bytes 兜底）。

## 0. TL;DR

- 现状：`SecretdataFS.writeFileOpt` 先 `io.ReadAll(r)` 把**整文件明文**读进内存（峰值 ≈ 1× 文件大小），
  再调 `EncryptShardsBytes` 逐块加密。**块大小上限（默认 1-200MiB）只约束单块临时密文，不决定整文件驻留**。
- 论证：全文驻留的三个理由里，**两个只需要「哈希值」而非「全文」**（Meta.SHA256、GroupSig），
  第三个（video-keyframe 关键帧解析）本就是 `io.ReaderAt` + 可 seek 语义，与流式**天然兼容**。
- 方案：为 shardseal 新增**基于 `io.ReaderAt` 的流式入口**（`EncryptShardsAt`），逐块 `ReadAt` + 流式哈希，
  内存峰值从 `O(文件大小)` 降到 `O(单块上限)`；**磁盘格式 / meta / 块布局 / 分组盲签 / 校验全部不变** → 向后兼容。
- 边界：**必须可 seek**（`io.ReaderAt`）。不可 seek 的纯流（如某些直传管道）保留现状 `ReadAll` 路径或
  落临时文件后转流式（见 §6 决策点）。

## 1. 现状与根因

### 1.1 写路径（`pkg/volume/secretdata/secretdata.go`）

```
writeFileOpt(ctx, rel, r, size, mtime, expected)
  ├─ size > MaxFileBytes ? → ErrMaxFileBytes（读取前拦截，默认 0 = 不限）
  ├─ data, _ := io.ReadAll(r)            ← 整文件明文驻留内存（1 × 文件）
  ├─ encryptContent(data, tmp, …)        ← EncryptShardsBytes（内存变体）
  │    └─ encryptShards → 逐块加密写 tmp/ 密文 blob + meta（每块独立写盘）
  ├─ uploadChunks(tmp → inner)           ← 逐 blob 读 tmp 再写底层卷
  └─ commitEntry（索引 + usage + 版本）
```

### 1.2 内存构成

| 项 | 大小 | 是否随块上限受控 |
|---|---|---|
| `data`（整文件明文） | O(文件大小) | **否**（io.ReadAll 一次整读） |
| 单块加密产物 `enc` | ≤ 块上限（默认 ≤200MiB） | 是 |
| tmp 目录全部密文 blob | O(文件大小) 磁盘，非内存 | — |
| 单块 blocklet 索引 / GCM | O(块上限) | 是 |

**关键**：块大小上限管的是「单块临时密文/写盘粒度」，**不影响 `data` 整驻留**——分块只是对已驻留的
`data` 做零拷贝切片视图。这就是「块加密、块大小有上限」仍存在整文件内存峰值的原因。

### 1.3 为什么现在整读（意图）

1. **API 简化**：入参直接是整块明文 `[]byte`，逐块 `data[b.Offset:…]` 切片，实现最快。
2. **整文件强哈希**：`Meta.Original.SHA256 = Hash256(data)`（全文 SHA-256）。
3. **分组盲签**：`GroupSig(secret, sha256(data))`（`HMAC(secret, SHA-256(整文件))` 48bit 窗口）——
   无 meta 时按中段盲分组恢复。
4. **video-keyframe blocklet 规划**：首个块触发 `parseOnce` 扫描整文件关键帧表（fMP4/GOP），
   需 `io.ReaderAt` 随机访问。

**结论**：① 是纯实现取舍；②③ 只需要「哈希值」可流式；④ 本就用 `ReaderAt`（§2.4 已内置），
不存在「必须全文常驻」的硬约束。`io.ReadAll` 的整读是**为实现的便利 + 大文件有 max_file_bytes 兜底**。

## 2. 可流式化论证（逐项）

### 2.1 整文件 SHA-256（Meta.SHA256 / GroupSig 用）

可**流式**计算：`sha256.New()` + `io.Copy` 一遍（或随逐块 `ReadAt` 增量写入 hash），
最终得到 `fullSum`。内存 O(1)。→ 不再需要全文驻留。

- `Meta.Original.SHA256`：`Hash256(fullSum[:])`（保持 64-hex 格式不变）。
- `GroupSig`：`HMAC(secret, fullSum[:])`（分组盲签值不变）。

### 2.2 块规划（RandomPlanner.Plan）

`Plan(origSize)` 只依赖**文件大小**，不碰内容（§block.go：`blocksPlanner.Plan(int64(len(data)))`）。
→ 流式可直接复用，不读任何块。

### 2.3 逐块加密（encryptWriteChunks）

`encryptBlocklets(key, salt, blocklets, chunk, v)` 只吃**当前块** `chunk = data[b.Offset:…]`；
`sealBlockletSegment` 用 `data[bl.Offset-blockOffset : …+Size]` 切片（§crypto.go）。
→ 把 `chunk` 从「整块切片」换成「`ReadAt(blockOffset, blockSize)` 读入的单块缓冲」，逻辑等价。

### 2.4 video-keyframe 关键帧解析

`VideoKeyframeBlockletPlanner.PlanBlocklets(data io.ReaderAt, …)` + `parseOnce(data, origSize)`
**本来就是 `io.ReaderAt`**；`KeyframeRequest{Path, Reader io.ReaderAt, Size}` 的接口注释
（planner_registry.go）写明「Reader 是整文件随机读」。ffprobe 实现有 `Path` 走文件路径，
go-mp4 走 ReaderAt。→ 流式下直接把「文件句柄 / 底层可 seek 视图」当作这个 `ReaderAt`，**零改动**。

> 注意：video-keyframe 的 `parseOnce` 在首块触发一次，会**读一遍整文件**（seek 扫描建关键帧表），
> 但它读的是**文件**不是内存——峰值仍是 O(块上限)，符合流式目标。

### 2.5 块内 blocklet 布局 / 索引 / 校验

`planBlockletLayout` / `buildBlockIndex`（`Hash16(data)` 整块摘要）/ `assembleBlockBlob` 都只依赖
**当前块**。→ 逐块流式天然兼容，块间无依赖。

## 3. 目标设计

### 3.1 新入口（shardseal 包）

```go
// EncryptShardsAt 流式分块加密：src 提供整文件随机读（可 seek），逐块 ReadAt + 流式哈希，
// 内存峰值 O(单块上限)。磁盘格式/meta/块布局与 EncryptShards/EncryptShardsBytes 完全一致（向后兼容）。
func EncryptShardsAt(src io.ReaderAt, size int64, origName, outDir string,
    secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error)
```

核心改动（在 `encryptShards` 基础上）：

```
1. salt/key 派生（不变）
2. blocks := blocksPlanner.Plan(size)            // 只依赖 size
3. fullSum := sha256.New()
     for _, b := range blocks {
         chunk := readAtExact(src, b.Offset, b.Size)   // 只读当前块进内存
         fullSum.Write(chunk)                          // 增量哈希
         …（video-keyframe parseOnce 用 src 而非 chunk——不变）
         encryptBlocklets(…, chunk, …) → blob → 写盘   // 逐块
     }
4. group := GroupSig(secret, fullSum.Sum(nil))
5. Meta.Original.SHA256 = Hash256(fullSum.Sum(nil))
6. encryptWriteMeta（不变，meta 加密不读全文）
```

> 分组盲签 `group` 原本在循环前（`fullSum := sha256.Sum256(data)`）→ 流式下挪到哈希收尾后。
> **磁盘上 blob 名 = 该 blob 的 Hash48Pair（首尾段）+ group（中段）**——group 变化会改变中段命名，
> 但这是**逐文件派生值**，与现有语义完全一致（同 secret+salt 同文件 hash → 同 group）。读路径按
> 现有解析规则（meta 锚定 + 组签）零改动。

### 3.2 secretdata 卷适配（writeFileEncrypted）— 修订版（2026-10-12 调用链审计后）

**零调用方改动原则（用户裁决：功能未上线，允许 API 直接变更）**：外部调用方全部保持传 `io.Reader` 不变。
seek 判定**收在 secretdata 内部** + 两个包装层透传 seek。

```go
func (s *SecretdataFS) writeFileOpt(ctx, rel, r io.Reader, size, mtime, expected int64, ifAbsent bool) error {
    if s.opts.MaxFileBytes > 0 && size > int64(s.opts.MaxFileBytes) { …ErrMaxFileBytes }
    // 流式优先：可 seek 且非 dedup/erasure（这两者需全文内存）
    if !s.opts.Dedup && !s.opts.Erasure {
        if _, ok := r.(io.Seeker); ok {
            return s.writeFileStream(ctx, rel, r, size, mtime, expected, ifAbsent)
        }
    }
    data, _ := io.ReadAll(r)   // 纯流/受限模式 → 现状回退（功能不变，仅内存不省）
    …现状路径…
}
```

`writeFileStream` 与 `writeFileEncrypted` 同构，仅加密入口换成 `EncryptShardsReader(r, size, …)`；
`writeCtx.data` 置 nil、新增 `wc.size`（commitEntry 的 usage/审计用 `wc.size` 而非 `len(wc.data)`）。

**调用链审计结论（决定性）**：所有真实链路的源头都可 seek（multipart.File 标准库保证
`io.Reader+ReaderAt+Seeker+Closer`；转存 `os.Open`；备份恢复/remote 写/volume_export 均文件/内存）。
“不可 seek”**只来自两层人为包装**：`io.TeeReader`（writeExternalSettle 算哈希）与 `rateLimitReader`
（带宽限速）。**必须透传 seek**（见 §5 实现）——否则 `writeFileOpt` 的 `io.Seeker` 断言恒失败，流式永不生效。

### 3.3 上层调用面（谁在喂 WriteFile）

- **Web/API 普通上传**（`pkg/files/write_ops.go` `writeExternalSettle`）：`src` 来自
  `mime/multipart.File`（底层 `*os.File`/`SectionReader`，可 seek）→ 流式路径天然命中。
- **分块上传 complete**（`chunked_upload.go`）：每块是 `multipart.File` → 同样可 seek。
- **云端转存 transfer**（`pkg/cloud/transfer.go` `writeTransferOnce`）：`os.Open(destPath)`
  本地文件 → 可 seek。
- **备份恢复 / sync / S3 / WebDAV**（旁路面）：均以文件/可 seek 流喂入 → 可 seek 判定覆盖。
- **纯流不可 seek**（罕见：流式 stdin 直传等）→ 回退现状 `ReadAll`（功能不变，仅内存不省）。

### 3.4 内存收益

| 场景 | 现状峰值 | 流式峰值 |
|---|---|---|
| 普通文件 5GB（默认块 1-200M） | ~5GB（`data`） | ≤ 200MB（单块）+ O(1) hash |
| 多并发大文件 | N × 文件大小 | N × 单块上限 |

`max_file_bytes` 从「唯一硬闸」变为「仍有用的文件级总闸」（防超大文件在 seek 判定前被读入）；
块上限 `max` 成为真正的**单块内存护栏**。

## 4. 向后兼容与回归

### 4.1 磁盘/读路径零改动

- **meta 结构**：`Meta.Original.SHA256 / GroupSig / Block / BlockletInfo` 字段、JSON 序列化格式
  与加密不变——新写入的 blob/meta 与旧版**逐字节同构**。
- **读路径**（`decryptRangeBlocklet` / `readChunkBlob` / `OpenRangeRead` / `Delete` / `Rename` /
  `restore`）：全部按现有 meta + blob 解析，不认识「流式写入」——无需改动。
- **分组盲签**：值语义与现状一致（同 secret+salt+内容 → 同 group），仅计算时机后移。
- **版本升级**：旧数据可读、新数据可被旧读路径读（写路径新版本只改内存布局，不改格式）。

### 4.2 语义保持

- `WriteIfAbsent` / `WriteFileIfVersion` / 覆盖写 / `usage` / 审计 `cipher_bytes` / `mtime` 打散
  逻辑全部在 `commitEntry` / `uploadChunks` 层，与流式无关。
- `max_file_bytes` 拦截时机不变（读取前按调用方 size）。

## 5. 实现清单（本分支落地，含包装层透传 seek）

| 文件 | 改动 |
|---|---|
| `pkg/cryptox/shardseal/shardseal.go` | 新增 `EncryptShardsReader(src io.Reader, size int64, …)`：内部 `io.ReaderAt`/`io.Seeker` 判定 → 流式（逐块 ReadAt + 增量哈希 + 组签收尾）；否则回退 `io.ReadAll` + 现有 `encryptShards` |
| `pkg/cryptox/shardseal/*` | `encryptWriteChunks` 改签名接受 `io.ReaderAt`（替代 `data []byte`）；块级 `ReadAt` + 增量哈希；`buildBlockIndex` 整块摘要用块缓冲；`PlanBlocklets(src)` 传 ReaderAt |
| `pkg/volume/secretdata/secretdata.go` | `writeFileOpt` seek 判定分叉；新增 `writeFileStream`；`writeCtx` 加 `size`；`commitEntry` usage 用 `wc.size` |
| `pkg/files/bandwidth.go` | `rateLimitReader` 实现 `Seek`/`ReadAt`（透传底层；ReadAt 不走限速，注释说明随机读语义） |
| `pkg/files/write_ops.go` | `writeExternalSettle` 用自定义 `teeReadSeeker`（Reader+Seeker+ReadAt，tee 哈希）替代 `io.TeeReader`——保留哈希校验且透传 seek |

**video-keyframe 模式（本期边界）**：`parseOnce` 需全文扫描（ReaderAt 读一遍），与 tee 哈希
（逐块仅一次）冲突 → **keyframe 模式本期保持 `io.ReadAll`**（现状路径，视频文件有 max_file_bytes 兜底）；
后续由 secretdata 内部返回哈希给上层解决（不在本期）。

## 6. 落地分期（建议）

| 阶段 | 内容 | 验证 |
|---|---|---|
| **S1**（本 PR） | 设计文档 + 内存基准（`memtest`）量化现状 | 数据佐证，无代码改动 |
| **S2** | `EncryptShardsAt` + `io.ReaderAt` 判定适配 `secretdata` 写路径 | 全量包测试 + `-race` + lint |
| **S3** | video-keyframe 模式流式专项（parseOnce 用 ReaderAt） | 视频关键帧 seek 测试 |
| **S4** | 旁路面（S3/DAV/sync/备份恢复）可 seek 判定覆盖 + 回归 | 各面 e2e |

> S2-S4 可合并为一个功能 PR，S1 可先行独立（纯文档 + 基准）。

## 7. 决策点（已按用户裁决确认）

1. **可 seek 判定范围**：含 `io.Seeker`（含 `io.ReaderAt`）——multipart.File / *os.File / SectionReader 全覆盖。
2. **不可 seek 回退**：直接走现状 `io.ReadAll`（不落临时文件）。
3. **保持 `EncryptShardsBytes`/`EncryptShards` 不变**（去重/测试/旧路径），新增 `EncryptShardsReader` 并行。
4. **`max_file_bytes` 保留**：文件级总闸，防 seek 判定前超大读入。
5. **API 直接变更许可**（用户裁决）：功能未上线，secretdata 内部签可自由调整（`writeCtx` 加 size 等），
   无需旧接口兼容。
6. **keyframe 模式本期保持 ReadAll**：parseOnce 全文扫描与 tee 哈希冲突，后续解决（见 §5）。

## 7. 风险与缓解

| 风险 | 缓解 |
|---|---|
| 块上限 200M 下大文件逐块 ReadAt I/O 放大 | 与现状整读 I/O 相同（都是读一遍文件），仅内存降；无 I/O 放大 |
| video-keyframe parseOnce 读全文（seek 扫描） | 本就存在（现状也对 ReaderAt 扫描），流式下内存不变 |
| 不可 seek 源内存不省 | 显式回退现状路径，功能不变（决策点 2） |
| `ReaderAt` 并发读底层文件的安全 | `*os.File.ReadAt` 线程安全；封装层加锁即可（见 S2 实现） |
