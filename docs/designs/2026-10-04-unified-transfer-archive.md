# 统一转存/归档 + 分块打包封装 设计

> 状态：**草稿，待审查**
> 日期：2026-10-04
> 关联：转存–归档统一（用户裁定 2026-10-04）、#34 组级打包转存、#733 视频关键帧 seek

## 1. 动机与结论

### 现状
- **转存 transfer**：单文件产物 → 目标卷（`transferFS` → `sync.FS`，含 secretdata 加密卷；`WriteIfAbsent` 防覆盖）
- **归档 archive**：多文件 → `io.Pipe + tar.Writer + compressx`（gzip/zstd/brotli）+ 可选 AES，输出**锁死** HTTP 流 / 租户 archive 桶

### 结论（用户裁定认可）
**归档 = 转存 + 可选预处理。** 两者底层都是「产物流(io.Reader) → 目标 writer」；打包是**预处理行为**（与写到哪无关），落盘是**转存行为**（与打不打包无关）。拆分更合理。

### 拆分
```
统一「转存」管道： 源(source) ─→ [可选预处理] ─→ 转存(destination 目标卷)
                        │                    │
                  读/解密         None(单文件原样)
                                  Pack(tar.gz / zstd / brotli)
                                  Pack+Encrypt(tar.aes)
                        └──► 可选解密（source 在 secretdata 卷）◄──┘
```

| 场景 | 预处理 | 转存目标 |
|---|---|---|
| 单文件直接转存（现 transfer） | 无 | 目标卷（任意 FS） |
| 多文件归档（旧 archive） | 打包 tar | 目标卷 / HTTP 流 / 桶 |
| 加密归档 | 打包 + 加密 | 目标卷 |

## 2. 目标行为（正交）

- **无预处理** = 纯转存（现状 transfer 语义）
- **有预处理** = 旧概念归档全升级为「打包后转存到任意卷」
- **行为正交**：打包与否不影响落盘；落盘到哪不影响打包
- **单一 code path**：`读 source → [预处理] → 写 destination`，复用 `transferFS`/`WriteIfAbsent`/ACL/配额/失败队列/secretdata 加密

## 3. 预处理层抽象

把打包从 archive handler 中抽出为可复用：

```go
// pkg/transfer 预处理器：产物流 → tar/压缩/加密 封装
type Preprocessor interface {
    // Pack 把 source 产出的一/多文件封装为单一聚合流的可选包。
    // Not: 本层不真实读取加密数据，只管「汇总相关数据文件的元信息 + 流组织」。
    Pack(ctx, sources []SourceEntry, w io.Writer) error
}

// 实现（可插拔，注册类似 volume/registry）
//  - nil            : 无预处理 = 纯转存（individual）
//  - tar.gz/zstd/brotli : PackPreproc（复用 compressx）
//  - tar.aes        : Pack+EncryptPreproc（复用 archiveCipher + compressx）
//  - shardseal 分块  : 见下（#34）
```

## 3. 图片分块加密（你补充的设想，纳入本次）

**你的设想（已确认）**：图片分块加密目前初步设想由 **secretdata 或加密包提供一种「打包封装」**：
- 提供**分块能力**，**不真实读写数据**，只是**汇总相关数据文件**
- 加密时**支持直接获取每个文件作为一个分块**（即每张图片 = 一个分块）

### 与 #733（视频关键帧 seek）同构
`shardseal` 已有 `BlockPolicy`/`MediaKindOf`（视频 mp4 关键帧分块 seek）。**图片组 = 把「每个文件当作一个分块」**——天然映射：
- 源 = 图片组（N 个文件）
- 分块 = 每个图片文件（granule = 单文件，非字节内 seek）
- 需求 = 随机访问**某一图片**（而非图片内字节）：`DecryptFile(meta, chunkDir, dst)` 已支持按分块解密

### 「不真实读写数据，只汇总」的含义
- 打包封装**只汇总元信息**：每文件的 `name,size,offset,mtime,分块索引` → 一个「档案 meta」（类似 shardseal Meta）
- **不真实读写**：打包阶段不整读文件内容（避免大内存）；分块规划 + 元数据先落
- **加密时支持直接取每文件**：将每个文件作为 block，加密后以分块形式存储；随机访问单图 = 按 index 解密该分块 + 解码 image

### 架构落点
两种等价实现方向（择一，登记待 #34 详设）：
- **A. secretdata 扩展**：新增「打包卷类型」——内层 = 现有 secretdata 卷；外层 pack 接口把「N 文件聚合为单 meta + 分块」挂到 secretdata 的索引模型（dirSeg 复用）
- **B. 加密包提供「打包封装」**：`shardseal.PackImages(entries, outDir, secret, policy)`——复用现有分块器，每个 entry 一 block；`OpenImage(index)` 随机取单图

> 本次**只登记方向 + 固化 API 界（接口/抽象）**，不实现字节层——真实数据读写、分块器都属 secretdata/shardseal 既有；先在传输层抽出「可插拔预处理」。

## 4. 实现拆分（步骤）

### 步骤 1（本期，并入 transfer-todos 分支）
1. 抽「预处理」抽象：`pkg/transfer` 新包，`Preprocessor` 接口 + registry（None/Pack*）
2. archive handler 输出改走「可插拔目标（writer）」——HTTP 流 = 一种 sink，保留零回归
3. archive/转存/上传统一走「源→预处理→目标卷」管道（ACL/配额/`WriteIfAbsent`/失败队列复用）
4. 行为正交 + 现有测试全绿 + 新增拆分测试

### 步骤 2（# 组级打包转存，待办）
- 组/批量下载完成后服务端打包 → 转存单一 tar 到卷（多文件 → consolidated）

### 步骤 3（图片分块，待办深设）
- 方向 A/B 选型 → 实现「图片组打包分块 + 随机访问单图」

## 5. 未决 / 需确认

1. 本次步骤 1 是否只做「抽象层 + archive 落盘收敛」，不带多文件打包转存（组级）？
2. 图片分块 A/B 方向本次仅登记，还是连接口都一并留在 transfer 包设计里？
3. content：source 是否也支持 secretdata 卷作为读取侧（当前主要 cloud bucket → 目标）？

## 6. 关键复用清单（防丢失，拿出评审）

| 能力 | 现位置 | 复用方式 |
|---|---|---|
| 目标卷解析 | `transferFS`/`volume/registry` | sink 统一 |
| 原子写不覆盖 | `sync.WriteIfAbsent`（W1/W3 新加） | 预处理目标写 |
| ACL | `volumeAllowedFor`/`checkTransferACL` | 全路径 |
| 配额/容量 | 全局 storageMgr + 租户 Scope + QuotaWriter | 统一记账 |
| 加密 | secretdata/shardseal | source 解密 / dest 加密 |
| 打包加密 | archiveCipherWriter / compressx | Pack+Encrypt |
| 失败队列 | 待重试 Pending + 标记 | 失败保留 |
| 分块随机访问 | `MediaKindOf`/`DecryptFile` | 图片单文件分块 |