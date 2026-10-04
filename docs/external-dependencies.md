# 外部依赖行为清单（测试锁定）

> **原则（2026-10-05 用户明示，AGENTS.md 硬规则 19 / CLAUDE.md）**：一切依赖外部系统/协议/服务的行为
> 必须**逐项测试锁定**——每项外部契约行为在对应测试中显式断言。新增外部依赖 → 先在本文档按分类
> 登记条目 + 补测试锁定（TDD 红灯 → 实现 → 绿）；测试红时**先对照本文档**判断「外部行为变化
> （依赖变更）」还是「实现回归」，禁止静默改实现适配未登记的依赖变化。外部行为确实变化 →
> 更新本文档 + 适配实现；实现回归 → 修实现，文档不动。**外部行为变化必须可见、可追踪、有锁，
> 绝不让其静默改变行为**。

## 1. PikPak 官方 CLI（v0.5.2，实测 2026-10-01）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `auth token` 是**保存命令**（`pikpak auth token <token>`），不再导出 access_token | API 鉴权改读 `.credentials.json`（`CredentialPath`）；不能依赖 CLI 导出 | `TestPikpakDownloader_Download*`（凭据文件装配） |
| `.credentials.json` 硬编码 `~/.pikpak/`（HOME/USERPROFILE/config-dir 无效） | 账号池 `Use` 必须写该路径会话切换才生效 | `TestNewAccountPool_DefaultCredDirIsPikpakHome` |
| CLI 读会话文件发现 access_token 过期 → 自动 refresh 并回写 | 服务端无需感知 token 过期（凭据文件恒最新） | 凭据文件装配测试 |
| 下载命令 `download <id> -o <out>` 直写目标文件 | `runCLI` 落盘后读回记账 | `TestPikpakDownloader_Download*` |

## 2. PikPak 匿名分享签名（gopeed 扩展同源，实测 2026-10-04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| captcha 签名 = `WEB_CLIENT_ID+VERSION+PACKAGE+deviceId+ts` 逐轮 MD5 × 15 盐 → `"1."+32hex` | 算法与 gopeed 扩展 `TeamBreakerr@gopeed-extension-pikpak/index.js` **完全一致**（纯 Go 移植，crypto/md5） | `TestCaptchaSign_Deterministic` / `TestCaptchaSign_DiffersByInput` |
| `captcha/init` 的 action 需 **`GET:/drive/v1/share`**（含路径） | action 错误 → HTTP 400 `invalid action`（实测踩坑） | `TestShareResolver_Resolve`（fake 校验 action） |
| device_id 32 位小写 hex（随机） | 签名随机性；重试同 device 复用 captcha token | —（实现细节） |

## 3. PikPak 匿名分享 API（drive/v1，匿名态，实测 2026-10-04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `GET /drive/v1/share?share_id=` 返回 `share_status=OK` + `files[]`（含 id/name/size） | 列分享内容；size 为 **string 数字**（sizex.ByteSize 兼容） | `TestShareResolver_Resolve` |
| `GET /drive/v1/share/file_info` 返回 `web_content_link` + `hash` + `medias[]` | 匿名直链来源；**hash 用于幂等校验**（P0-1） | `TestShareResolver_Resolve` |
| 分享 URL 形态：`mypikpak.com/s/<id>` 与 **`keepshare.org\|cc/<id>/magnet:...`** | keepshare 镜像 301 → keepshare.cc（实测）；镜像形态 parts[0] 直接是 id | `TestParseShareID_KeepshareCC` |
| 分享链接带**子路径**（`/s/<id>/<子路径key>`） | `parseShareID` 取 `<id>` 忽略子路径 | `TestParseShareID_SubPath` / `TestParseShareID_KeepshareCC`（隐式） |

## 4. PikPak REST API（drive/v1，账号态，实测 2026-10-01/04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `FileMeta.Size` 返回 **string 数字**（`"12893054"`）或人类可读（`"12.30 MB"`） | `FileMeta.UnmarshalJSON` 兼容 string/number（复用 `sizex.ParseSize`） | `TestFileMeta_SizeForms` |
| 分享链接下载限制（40-50%，官方反下载设计） | 中转必须「转存个人网盘 → CLI 完整下载」（或 hybrid 分享区≤50%） | —（设计文档） |
| 自分享文件 restore 返回 `file_restore_own`（错误码 9）→ 返回源文件 ID | 已在个人网盘 → 直接定位下载；**AutoDelete 不得删源文件**（NH-P1） | `TestRestoreShare_OwnFile` / `TestPikpakDownloader_AutoDelete_SkipsOwnedFile` |
| **转存 restore 返回「Pack From Shared 文件夹」id**（非文件） | locateRestored 列文件夹找视频文件（真实踩坑） | `TestHybridDownload_RestoreReturnsFolder` |
| **RESTORE_START 异步**（文件稍后进网盘） | locateRestored FindByID 可能暂 miss | —（轮询/重试） |
| **转存不幂等**（多次 restore 累积同名副本 (1)(2)(3)） | restore 前 FindInDrive + **Hash 校验**跳过重复（P0-1） | `TestHybridDownload_RestoreIdempotent` / `TestHybridDownload_RestoreHashMismatch` |
| `GET /drive/v1/files/{id}?usage=FETCH` 返回 `web_content_link`（**全 Range 206**） | 账号区续传基础；分享 416 后转账号区 | `TestHybridDownload_ShareAndAccount` |
| 单连接限速 ~1.3MB/s，并发不加速 | 账号区性能 = 单连接（多账号并行才是提速） | —（实测，设计文档 §8） |
| `POST /drive/v1/files:batchDelete` **永久删除**（释放配额空间，实测 usage→0） | AutoDelete 用永久删（batchTrash 只移回收站不释放） | `TestAPI_DeletePermanent` |
| 删除是**异步任务**（返回 task_id，PikPak 端清理数秒） | 空间释放有延迟；hybrid 完成后删除不阻塞 | —（实现细节） |

## 5. 分享直链行为（dl-*.mypikpak.com，CDN，实测 2026-10-04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| **Range 0~55% 可下（206），之后 416**（官方反下载设计） | 分享区恒 ≤50%（shareRatio 钳位）+ **probeBoundary 探测实际边界** | `TestProbeBoundary` × 2 |
| **单连接限速 ~1.2MB/s，并发不加速**（CDN 级） | 分片并发无法突破；分享区价值 = 免账号配额 | —（实测，设计文档 §8） |
| **需 Referer: https://mypikpak.com/**（CDN 校验） | 缺失可能 403 | `TestHybridDownload_RangeRequestHeaders` |
| **expire 签名短时有效**（~1h，过期 403） | chunk 失败需重取直链（resolve 新签名） | `TestHybridDownload_ShareChunkFails_DowngradesToAccount`（重试路径） |

## 6. 免费账号限制（实测 2026-10-04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| **网盘空间 6GB**（limit=6442450944） | 转存大文件失败（7.16GB 实测 FILE_SPACE_NOT_ENOUGH）；必须 AutoDelete 及时释放 | —（实测） |
| **带宽 ~1.2MB/s**（所有通道共享 CDN 限速） | 分片/并发无法突破；多账号并行是唯一提速 | —（实测，设计文档 §8） |
| 日下载配额 20GB（AccountPool.DefaultDailyQuota） | 多账号轮换按日配额调度 | —（AccountPool 既有） |

## 7. 本仓库内部契约（自研行为，测试锁定）

### 7.1 卷注册协议（volume/registry）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| 后端 `RegisterBackend(typ, factory, protocols...)` 声明 scheme | 转存 URL 生成依赖 `SchemeOf(typ)` 反查 | `TestSchemeOf_AfterProtocolRegistration` |
| `ResolveURL(scheme://卷/路径)` 按 scheme 查表定位类型 | 转存产物经此取用（secretdata 解密读回） | `TestResolveURL_*` |

### 7.2 云端下载任务行为（cloud）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| 转存目标卷前置校验（创建即拒未装配/无协议） | 避免浪费下载 | `TestCreateTask_TransferVolumePrecheck` |
| 共享卷 owner 前缀隔离 | 内容不共享，落盘加 owner 前缀 | `TestTransferDone_SharedVolume_*` |
| save=false 服务端自动清理 + 审计 | 客户端异常不残留 | `TestTransferAfterDownload_SaveFalse_*` |
| 下载本地告知（download_local） | 正交性（只转存跳过拉取本地） | `TestCloudDownloadChain_NoDownloadLocal_*` |

### 7.3 Hybrid 下载器内部（pikpak hybrid）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `runChunks` 分享区/账号区**分池并行**（互不阻塞） | 分享直链限速不拖账号区 | `TestHybridDownload_ShareAndAccount`（分片规划） |
| **manifest**（destPath.hybrid）记录完成 chunk | 崩溃恢复跳过已完成（分享区免配额不浪费） | `TestHybridManifest_Resume` / `TestHybridDownload_ResumeKeepsCompletedChunks` |
| **sink 配额记账**（DownloadWithWriter 完成后重放） | 对齐内置 HTTP 下载器配额语义 | `TestHybridDownload_SinkAccounting` |

## 维护指引

- **新增外部依赖**：先在本文档对应分类补条目 + 测试锁定（TDD 红灯 → 实现 → 绿）
- **测试红时**：先对照本文档判断「依赖变化（外部改了）」vs「实现回归（我们改了）」
- **依赖变化处置**：外部行为变化 → 更新本文档 + 适配实现；实现回归 → 修实现，文档不动
- **静默变更拦截**：外部行为一旦无锁即会静默漂移（例：416 边界 / expire 签名 / keepshare 301
  / file_restore_own）——任何实现改动了上述任一行为，必须先改本文档对应行再动代码。
