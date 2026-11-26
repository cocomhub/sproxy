# 文件 Meta 存储一致性

> 目标：客户端通过同一套 `/api/files/meta`、`X-File-Meta-Extra`、`GetFileMetaInfo` 看到一致效果，
> 不因服务端卷类型（local / secretdata / baidupcs / s3 / webdav / ftp / sftp）而分叉。

## 1. 统一语义

以下能力对所有**可写卷**一致：

| 能力 | 统一语义 |
|---|---|
| 上传 `X-File-Meta-Extra` | 合并进该文件的 FileMeta.Extra（白名单校验、大小上限） |
| 上传 mtime | 写入 FileMeta.MTime；未提供时记录上传时刻 |
| `PUT /api/files/meta` | 读-校验-合并-写；默认 merge，支持 replace/delete |
| `GET /api/files/meta` | 返回 `extra` + `mtime` + `ctime` |
| 下载校验 | 有 FileMeta 时按整文件/分块哈希校验；缺失时按既有降级语义 |
| 重命名/复制/移动 | sidecar 或卷内 meta 随主文件联动 |

## 2. 各卷实现位置

| 卷类型 | Meta 载体 | 读 | 写 |
|---|---|---|---|
| local | `meta/<rel>.meta` sidecar（`filesMetaPolicy`） | `FileMetaPolicy` | 本地写内核 + sidecar |
| external（baidupcs/s3/webdav/ftp/sftp） | `trusted.Wrap` 的 `meta` 桶 sidecar | `meta.Provider` | `TrustedVolumeFS.WriteFile/WriteIfAbsent` |
| secretdata | `shardseal.Meta` 加密卷内 meta | `SecretdataFS.FileMeta` | `SecretdataFS.writeFile` |
| enclosure | 封装层 `meta.Provider` | 封装层 | 封装层 |

关键点：

- `VolumeView(name, ViewUser)` 是唯一取卷入口；外部卷统一套
  `trusted.Guard(trusted.Wrap(...))`，所以外部卷不是「无 meta」。
- secretdata 自带 `meta.Provider`，`trusted.Wrap` 短路，不重复装饰。
- `WriteTarget` 的外部卷分支必须把 `WriteTargetInput.MetaExtra` 放进
  `syncpkg.WithUploadMetaExtra(ctx, ...)`；否则普通 multipart 上传会在外部卷
  静默丢失 `X-File-Meta-Extra`（本文件对应的修复点）。
- `TrustedVolumeFS.WriteFile`/`WriteIfAbsent` 必须同时写入 `Extra`、`MTime`、`CTime`。
  只写哈希不写时间会让客户端在外部卷读到空 mtime，而本地/secretdata 有值。

## 3. 已知边界

- **presign 直传**（内容由客户端直传对象存储，服务端只登记）：
  - local 卷在登记时仍写 sidecar；
  - 外部卷（trusted.Wrap）在 complete 时按客户端声明的 size+整文件 sha256 写
    **单块 FileMeta**（不重读内容）；`X-File-Meta-Extra` / `X-File-MTime` 随 complete
    请求写入 sidecar。单块 meta 提供整文件校验，不提供细粒度分块定位；
  - secretdata 等无法凭明文 hash 生成卷内加密 meta 的后端不支持直传登记，需走普通上传；
  - `sclient upload-direct --meta-extra` 已支持该登记。
- `direct_link: true` 的外部卷下载可能绕过服务端转发；这是显式的直链语义，不承诺
  服务端 meta 校验，保持现有安全边界。

## 4. 回归

- `pkg/volume/trusted`: sidecar 生成 + ctx `X-File-Meta-Extra` + mtime/ctime。
- `pkg/files`: 外部卷写面 ctx extra 透传。
- `pkg/volume/secretdata`: 卷内 meta extra + mtime。
- `pkg/server`: 外部卷 meta API/Provider 分支、S3 trusted sidecar。
