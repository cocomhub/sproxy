<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secretdata（加密卷）CLI 文件面收口 — 归档

> **归档说明**：原文 `docs/designs/2026-10-13-secretdata-cli-face.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/secret-volume.md`（§8.2 CLI 文件面 / §9 已知限制）与 `docs/cli.md`（卷 URI 寻址、
> 卷感知工作目录、`cp`）。本文件只保留**决策 / 语义不变量 / 踩坑 / 仍有效边界 / 落地指针**。

## 1. 起点：CLI 面对加密卷的静默错误（修复前实测）
① `sclient --volume vault list` **静默返回空**（`files.List` 走搜索索引，只遍历本地 `storage.Root`，
外部/加密卷无 Root ⇒ 未列举且不报错）；② `mv` 恒失败「源文件 checksum 为空」（`StatPath` 外部卷分支
不填 checksum）；③ `RenameFile` 只覆盖本地卷；④ `SecretdataFS.Rename` 对文件改 basename fail-closed。
⇒ 教训：**「200 但空」是最难查的一类**（用户以为卷是空的）。

## 2. 决策与语义（仍有效）
- **列举**：显式 `?volume=` 时优先经接缝 `ExternalVolumeOps.ExternalList` 列举外部卷（同一可信链
  `Guard(Wrap(be.FS()))` + `ResolveUserLocation` 键翻译）；非外部卷落回索引语义（零回归）。
  **未指定卷时不触达外部卷**（避免每次列表打远端 API）⇒ 记为已知限制。
- **checksum**：新增可选能力 `files.ChecksumSource`——`StatPath` 外部卷分支对实现者填明文整文件
  SHA-256（来自 `meta.Provider`）；未实现 → 保持旧行为（空，**不伪造**）。
- **改名 = 只重写 meta**：文件名只存在于加密 meta 内（`Original.Name`；meta blob 名派生自密文内容）
  ⇒ 改名**换名重写 meta、分块内容零改动、不重加密**（分块名 = 密文内容哈希，与逻辑名无关）；
  **先写新 meta 成功、再删旧 meta**（崩溃窗口最多两份 meta，不丢数据）；耗时 **O(meta)** 而非 O(内容)。
  契约哨兵 `syncpkg.ErrUnsupported` + 装配层「读源→写目标→删源」兜底保留给其它后端。

## 3. 同卷复制：底层 blob 直拷 + 「存在即引用」（用户裁定）
分块 blob 名 = **密文内容哈希**，文件名只在 meta 内 ⇒ 复制 = 写一份新 meta（换 basename，沿用原 `Salt`
⇒ 内容密钥与分块都不变）：

| 情形 | 处理 | 代价 |
|---|---|---|
| **同容器** | 分块**原地共享**，只写新 meta | **零 blob 写、零加解密**，O(meta) |
| **跨容器** | 分块（+parity）字节拷贝到目标容器 + 新 meta | O(内容) 字节拷贝，**零加解密** |
| 跨卷 | 目标卷有独立密钥 ⇒ 必须重新加密 | O(内容) 加解密 |

- **引用计数 = 「存在即引用」（无新元数据字段）**：共享事实就是「两份 meta 引用同一批 blob」本身 ——
  不写标记、不复用 `Extra["dedup"]`/`dataDir`（那是为**预留去重池**设计的）。删除时统一裁决：
  **仅最后一个引用者**才物理删分块+parity ⇒ 源与副本谁先删都安全；**重启由索引重建天然恢复**。
- **配额不可旁路**：blob 直拷不经写内核 ⇒ 域侧在调卷能力前经 `routeWriteUpload` 做与上传**同一组原语**
  的 owner Scope + 卷容量池**双预留**，成功 `Commit`、失败/回落 `Release()` ⇒ 与写入不可分叉。
  同容器共享时**物理字节不增**但**逻辑占用按文件大小记账**（物理真值由 `TrueUsage`/周期对账收敛）。

## 4. 卷 URI 寻址 + 卷感知工作目录（用户口径）
- 位置参数 `<卷名>:/<路径>`（`<卷名>:` = 卷根）等价 `--volume <卷名>` + 纯路径；**纯客户端归一**
  （只改本地参数/flag；线上仍是 `?volume=`，协议/服务端零改动）。`splitVolumeURI` 要求 `:/`（冒号后
  单斜杠）⇒ `https://` 与 `C:\` 不误判；位置参数与 `--volume` 卷名冲突 → **报错**（不静默取其一）。
- **URI 路径语义（踩坑）**：`splitVolumeURI` 返回**保留前导 `/`** 的绝对路径 ⇒ 绕过 `cd` 前缀
  （否则 `list vault:/` 会被当「当前目录相对」）。
- 工作目录支持卷 URI（`cd A:/path`）；所有操作默认只作用于**当前工作目录所在卷**；`/` 开头 = 本地卷根。
  优先级：**位置参数卷 URI > `--volume` > 工作目录所在卷 > auto**。

## 5. 跨卷移动 + 加密卷目录删除
| 能力 | 实现 | 关键点 |
|---|---|---|
| 跨卷移动（含外部卷） | 装配层判「涉及非本地卷」→ 域侧 `files.Service.MoveFile` | 复制复用 `CopyFile`；**写成功才删源**；删源外部卷 `ExternalDelete` 硬删（带复制时明文 checksum 交叉比对，防复制后源被改写仍被删）；删源 404 幂等 |
| 加密卷删目录 | 接缝 `ExternalVolumeOps.ExternalRemoveDir` + 卷可选能力 `SecretdataFS.RemoveDir` | 装配层**递归删**（先子后父、深度上限 64）；发往卷的删除**先经装饰器链 `Delete`**（封印不被绕过），再取**最内层**卷 `RemoveDir` |

## 6. 踩坑族（仍有效）
- **能力断言必须穿透装饰器**：装配层 `be.FS()` 通常是装饰器（CapacityFS/trusted/Guard）⇒ 直接类型断言
  `CopyFile`/`RemoveDir` 会**假阴性**（静默回落 = 白重加密/静默退化）。改用 `syncpkg.Innermost(raw)` 后断言。**复发过两次**。
- **CLI `rmdir` 从不发送 `force=true`**（`--force` 只跳本地提示）⇒ 服务端恒要 `force=true` ⇒ CLI `rmdir`
  （含 `--force`）**恒 400**。修：`FileClient.Rmdir(ctx, dirname, force)` 透传；列目录失败且未 `--force` 时**拒绝删除**（不猜）。
- **AD-4 唯一性查重把移动的源卷自身判成冲突**：跨卷移动写入若走「显式卷路由（带查重）」⇒ 因**源卷**
  持有同 rel 而 409。修：移动写入改用**统一写内核 `WriteTarget`（不查重）**，由域侧 `moveUniqueConflict`
  按**源卷豁免**口径复刻 AD-4。
- **客户端 `Mkdir`/`Rmdir` 手拼 URL 漏带 `volume` 查询参数** ⇒ `--volume`/URI 静默失效（mkdir 落到本地卷）。
- **`files.Service.MakeDir` 无卷参数** ⇒ 显式卷被忽略；`rmdir` 显式外部卷须 **fail-closed 501**（此前会静默删本地同名目录——危险方向）。
- **`list --json` 空结果此前输出空串**（`JSONFormatter.Println` 被忽略）⇒ 非法 JSON；改为 `{"files":[],"total":0}`。
- **部分 Range 被误杀**：加密卷上「从 0 起但不覆盖整文件」的 Range（`bytes=0-1023`）被 `verifyEOF` 误判
  截断 ⇒ 播放器从 0 seek 的 Range 全被误杀。修：`expectEnd>0 && expectEnd<fm.Size` 时不再套用短读/整文件哈希判据。

## 7. 落地指针
- `pkg/volume/secretdata/secretdata.go`（`Rename`/`RemoveDir`/`CopyFile`）、`pkg/files/copy_ops.go`、`read_ops.go`、
  `pkg/server`（`ExternalVolumeOps` 接缝族、`copyThenDeleteExternal`）、`cmd/sclient/volume_uri.go`、`cmd/sclient/cp.go`。
- e2e：`TestE2E_CLI_Secretdata_CliFileOps`、`TestE2E_CLI_VolumeCwd_DefaultsToCurrentVolume`、
  `TestE2E_CLI_Xfer_ExternalVolumeMoveAndRmdir`、`TestE2E_CLI_Cp_QuotaReservedOnSameVolume`。
