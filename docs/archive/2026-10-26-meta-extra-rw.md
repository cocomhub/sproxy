<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 文件元数据 extra 完整方案（读写闭环 + 全卷覆盖）— 归档

> **归档说明**：原文 `docs/designs/2026-10-26-meta-extra-rw.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/api.md`（`PUT /api/files/meta`、`GET /api/files/meta`）与 `docs/cli.md`（`sclient meta`）。
> 本文件只保留**语义裁定 / 全卷覆盖 / 定位模型 / 一致性红线 / 边界 / 落地指针**。

## 1. 起点：只有「写」没有「读」
`X-File-Meta-Extra` 写 `extra` 已可用，但**无任何读端点/SDK/CLI/UI**（核心缺口）；且**语义不一致**：
上传路径 `applyMetaExtra` 是**覆盖**（第二次上传同文件丢旧 KV），补录路径（`UpdateMetaExtra`）是**合并**。

## 2. 语义裁定（用户 2026-10-07 明示）
> **常规默认应「新增（合并）」，覆盖是显式动作。**

| 操作 | 语义 |
|---|---|
| 上传（单次/分块）带 `meta_extra` | **合并**（默认新增，不覆盖旧键）⇒ `applyMetaExtra` 改 `maps.Copy` |
| `meta set --extra`（补录） | **合并**（同名键新值覆盖旧值 = 合并的天然行为） |
| `meta set --extra --replace` | **整表替换**（删旧键）——显式覆盖形态之一 |
| `meta set --delete=k1,k2` | 删指定键（保留其余）——显式覆盖形态之二 |

服务端在 `PUT /api/files/meta` body 加 `mode: "merge"|"replace"|"delete"` + `keys: [...]` 收敛。

## 3. 定位模型（关键：双路，非单一 `locateForRead`）
`UpdateMeta`（补录）原用 `s.locateForRead(owner, rel, "")` ——该函数**只扫本地 `*storage.Root`**
（`volumeFileExists` = `vs.Root(name).Stat`）⇒ **外部卷（secretdata/baidupcs/s3）`Root` 为 nil
⇒ 恒未命中 ⇒ `meta set` 到加密卷/网盘卷根本无法工作**（不只是路由 500）。

**正确模型（已有先例）**：外部卷定位必须走 `ExternalVolumeOps`（`externalVolOps()` 接缝）——
`copyWithinVolume`（`pkg/files/copy_ops.go`）与 `listExternalVolume`（`read_ops.go`）都是
「`HasLocalRoot` 为 false ⇒ 走 `ops.ExternalStat`/`ExternalList`」的双路模型。**meta 读/写必须补齐同构双路**。

读侧：按卷 FS `meta.Provider` 探测（`fileMetaReaderFor` 同链）→ `FileMeta.Extra`（**深拷贝防污染**）；
sidecar 缺失 = 无 extra ⇒ `200 {extra: {}}`（读侧零校验语义，与 `warnDegradeDebug`「缺失 = 直通」一致）。

## 4. 全卷覆盖矩阵与边界
| 卷类型 | 读（FileMeta） | 写（上传带 extra） | 写（meta set 补录） |
|---|---|---|---|
| 本地卷 | trusted.Wrap FileMeta ✅ | applyMetaExtra（改合并）✅ | UpdateMetaExtra ✅ |
| 加密卷（secretdata over local/网盘） | Provider ✅ | **UploadSink 链路补 extra**（ctx 承载） | ✅ |
| **裸外部卷（baidupcs/s3，无 trusted）** | ⚠️ 无 sidecar ⇒ 读返回空 | ⚠️ 无法持久化 | ⚠️ `ErrMetaUnavailable` |
| 封装卷（enclosure） | 底层 FS 转发 ✅ | 底层透传 ✅ | 底层 ✅ |
| 去重卷 / 备份同步 | 底层 ✅ | 上传 extra 落 meta ✅ | 转发 ✅ |

- **裸外部卷（无 trusted）明示 fail-closed**：sidecar 载体缺失 ⇒ 读返回空 / 补录返回明确「不支持」，
  **不做静默假成功**（与 `ErrMetaUnavailable` 语义一致）。用户需要时再评估「sidecar 放网盘同目录」方案（独立决策）。
- **外部卷上传 extra 不丢（关键缺口修复）**：`UploadSink.WriteFile` 签名无 extra 参数 ⇒ 外部卷分块上传
  的 `meta_extra` 被丢弃。修法：经 `ctx`（`syncpkg.WithMetaExtra`/`UploadMetaExtraFrom`）承载
  （R2 门禁：files 域不直接 import pkg/sync ⇒ 走 ctx），写侧（trusted/secretdata）合并。

## 5. 一致性红线（用户 2026-10-07）
补录/上传的 extra 写入**不得改变**主文件校验链（TotalSHA256/分块）：合并只改 `fm.Extra` 字段，meta
重算时沿用主文件哈希（trusted 保证更新前校验主文件未变）；加密卷 meta 重加密沿用原 chunks/哈希字段——
**只改 Extra**。读侧不做 Trim/Validate（写侧已把关）。

## 6. 落地指针
- 域：`pkg/files/meta_update.go`（`GetMeta`/`UpdateMeta` 双路定位）、`pkg/files/meta/extra.go`（schema）、
  `pkg/sync/metaextra.go`（`UploadMetaExtraFrom`）。
- 服务端：`pkg/server/files_meta_handler.go`、`routes.go`（`isFileGroupedRoute`+`isReadOnlyFileRoute`+`HandleFunc`
  三处同源——#855 教训）。
- SDK/CLI/Web：`pkg/client/upload_meta_extra.go`（`GetFileMeta`/`WithMetaExtra`）、`cmd/sclient/meta.go`、
  Web UI 详情/编辑面板（`node --test` 纯函数 + Playwright e2e）。
- 已知尺寸约定：`max_file_meta_bytes` 默认 **10 MiB**（`extra` 完整 JSON 序列化后上限同此）。
