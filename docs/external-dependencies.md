# 外部依赖行为清单（测试锁定）

> 本文档列出本项目依赖的外部系统/协议行为，每项均有**测试锁定**（防止随维护丢失信息）。
> 若外部系统行为变化导致测试红，先对照本文档判断是「依赖变化」还是「实现回归」。

## 1. PikPak 官方 CLI（v0.5.2，实测 2026-10-01）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `auth token` 是**保存命令**（`pikpak auth token <token>`），不再导出 access_token | API 鉴权改读 `.credentials.json`（`CredentialPath`）；不能依赖 CLI 导出 | `TestPikpakDownloader_Download*`（凭据文件装配） |
| `.credentials.json` 硬编码 `~/.pikpak/`（HOME/USERPROFILE/config-dir 无效） | 账号池 `Use` 必须写该路径会话切换才生效 | `TestNewAccountPool_DefaultCredDirIsPikpakHome` |
| CLI 读会话文件发现 access_token 过期 → 自动 refresh 并回写 | 服务端无需感知 token 过期（凭据文件恒最新） | 凭据文件装配测试 |
| 下载命令 `download <id> -o <out>` 直写目标文件 | `runCLI` 落盘后读回记账 | `TestPikpakDownloader_Download*` |

## 2. PikPak REST API（drive/v1，实测 2026-10-01/04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `FileMeta.Size` 返回 **string 数字**（`"12893054"`）或人类可读（`"12.30 MB"`） | `FileMeta.UnmarshalJSON` 兼容 string/number（复用 `sizex.ParseSize`） | `TestFileMeta_SizeForms` |
| 分享链接带**子路径**（`/s/<id>/<子路径key>`） | `parseShareID` 取 `<id>` 忽略子路径 | `TestParseShareID_SubPath` |
| 自分享文件 restore 返回 `file_restore_own`（错误码 9） | 已在个人网盘 → 返回源文件 ID 直接定位下载 | `TestRestoreShare_OwnFile` |
| 分享链接下载限制（40-50%，官方反下载设计） | 中转必须「转存个人网盘 → CLI 完整下载」 | —（设计文档） |

## 3. 卷注册协议（volume/registry，本仓库内部契约）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| 后端 `RegisterBackend(typ, factory, protocols...)` 声明 scheme | 转存 URL 生成依赖 `SchemeOf(typ)` 反查 | `TestSchemeOf_AfterProtocolRegistration` |
| `ResolveURL(scheme://卷/路径)` 按 scheme 查表定位类型 | 转存产物经此取用（secretdata 解密读回） | `TestResolveURL_*` |

## 4. 云端下载任务行为（cloud，本仓库契约）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| 转存目标卷前置校验（创建即拒未装配/无协议） | 避免浪费下载 | `TestCreateTask_TransferVolumePrecheck` |
| 共享卷 owner 前缀隔离 | 内容不共享，落盘加 owner 前缀 | `TestTransferDone_SharedVolume_*` |
| save=false 服务端自动清理 + 审计 | 客户端异常不残留 | `TestTransferAfterDownload_SaveFalse_*` |
| 下载本地告知（download_local） | 正交性（只转存跳过拉取本地） | `TestCloudDownloadChain_NoDownloadLocal_*` |
