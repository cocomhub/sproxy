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

## 3. PikPak 匿名分享 API（drive/v1，匿名态，实测 2026-10-04/05）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `GET /drive/v1/share/detail?share_id=` 返回 `share_status=OK` + `files[]`（含 id/name/size） | 列分享内容；**2026-10-05 实测旧 `/drive/v1/share` 不返回子目录文件，需 `/share/detail`**（与 pikpak.exe `share get` 抓包一致）；size 为 **string 数字**（sizex.ByteSize 兼容） | `TestShareResolver_Resolve` / `TestShareResolver_SubfolderRecursive` |
| `GET /drive/v1/share/detail?parent_id=<folderID>` 列**子文件夹**内容 | 分享文件夹型分享的递归遍历（根返回 `drive#folder` 对象，需 `parent_id` 深入）；API 可能重复返回同一 folder → 递归需去重防死循环 | `TestShareResolver_SubfolderRecursive` / `TestShareResolver_SubfolderDedup` |
| `GET /drive/v1/share/file_info` 返回 `web_content_link` + `hash` + `medias[]` | 匿名直链来源；**hash 用于幂等校验**（P0-1）；folder 无直链（file_info 对 folder 返回错误） | `TestShareResolver_Resolve` / `TestShareResolver_SubfolderDedup` |
| 分享 URL 形态：`mypikpak.com/s/<id>` 与 **`keepshare.org\|cc/<id>/magnet:...`** | keepshare 镜像 301 → keepshare.cc（实测）；镜像形态 parts[0] 直接是 id | `TestParseShareID_KeepshareCC` |
| 分享链接带**子路径**（`/s/<id>/<子路径key>`） | `parseShareID` 取 `<id>` 忽略子路径（token 提取保留给 share_detail 透传） | `TestParseShareID_SubPath` / `TestShareResolver_SubfolderRecursive` |

## 4. PikPak REST API（drive/v1，账号态，实测 2026-10-01/04）

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `FileMeta.Size` 返回 **string 数字**（`"12893054"`）或人类可读（`"12.30 MB"`） | `FileMeta.UnmarshalJSON` 兼容 string/number（复用 `sizex.ParseSize`） | `TestFileMeta_SizeForms` |
| 分享链接下载限制（40-50%，官方反下载设计） | 中转必须「转存个人网盘 → CLI 完整下载」（或 hybrid 分享区≤50%） | —（设计文档） |
| 自分享文件 restore 返回 `file_restore_own`（错误码 9）→ 返回源文件 ID | 已在个人网盘 → 直接定位下载；**AutoDelete 不得删源文件**（NH-P1） | `TestRestoreShare_OwnFile` / `TestPikpakDownloader_AutoDelete_SkipsOwnedFile` |
| **转存 restore 返回「Pack From Shared 文件夹」id**（非文件） | locateRestored 列文件夹找视频文件（真实踩坑） | `TestHybridDownload_RestoreReturnsFolder` |
| **RESTORE_START 异步**（文件稍后进网盘） | locateRestored FindByID 可能暂 miss | —（轮询/重试） |
| **转存不幂等**（多次 restore 累积同名副本 (1)(2)(3)） | restore 前 FindInDrive + **Hash 校验**跳过重复（P0-1） | `TestHybridDownload_RestoreIdempotent` / `TestHybridDownload_RestoreHashMismatch` |
| **restore 副本位于「Pack From Shared」文件夹**（实测） | AutoDelete 判别器：仅该形态登记删除；用户自有文件（自分享）只复用不删（NH-P1 数据丢失防护，2026-10-05） | `TestHybridDownload_IdempotentHit_UserFileNotDeleted` / `TestHybridDownload_IdempotentHit_RestoreCopyDeleted` / `TestHybridDownload_OwnedRestore_NotDeleted` |
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

### 7.4 BaiduPCS-Go 上传路径（真实网盘，e2e 门控）

> **入口**：`cd pkg/volume/ext/baidupcs && go test -tags=e2e -run TestE2E_CompareUploadPaths -count=1 -v`
> **前置**：WSL 已登录 BaiduPCS-Go（`~/.config/BaiduPCS-Go/pcs_config.json`）且二进制可用
> （PATH 或 `~/go/bin/BaiduPCS-Go`，经 `os.UserHomeDir()` 动态解析不硬编码用户名）；
> 前置不满足 t.Skip。**凭据运行时读取、不经转录/落库**。

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| >4MB 文件分片上传后 `Stat.MD5` 是片组合/服务端"可能不正确"（非整文件 md5） | 分片上传 ETag 语义（C-C1 前提，WSL 实测 2026-10-07：9MB 本地 `8f566ecd` vs 远端 `18df0a1a`） | `TestE2E_CompareUploadPaths`（两条路径最终都须收敛 ETag==本地 md5） |
| CLI 二进制 `upload`：内部自动完整块列表秒传 → 收敛快 | 二进制路径性能基准 | 同上（对比结果输出：耗时/上传次数/ETag 权威） |
| 库兜底（libraryAdapter + refreshByRapidUpload）：秒传刷新用**真实分块 md5 列表**（blockMD5s）→ 命中刚上传块索引 → md5 权威 | 库路径收敛机制（C-C1 修复：原 `RapidUploadNoCheckDir` 只发整文件 md5，>4MB 恒 miss） | 同上（对比结果输出） |
| 生产默认装配（binaryAdapter 二进制优先 + 库 Fallback）：refreshByRapidUpload **透传 Fallback** 秒传能力 | 默认路径大文件收敛（C-C1 深化，与 metadata() 透传同构） | `TestRefreshByRapidUpload_FallbackBinary`（单测锁定） |

### 7.5 BaiduPCS-Go 元信息子命令（`meta` / `ls`，binary-only 免下载）

> **背景（2026-10-10）**：binary-only 模式（无 BDUSS/库会话）下 `Stat` 此前回退「下载整文件
> 到本地再 stat+md5」——`Put` 前后各一次全量下载，最坏 ~4× 文件流量。现改为解析 CLI 元信息。
> **入口（WSL 实测）**：`BaiduPCS-Go meta <path...>`（精确字节 + md5 + mtime + 类型，
> 支持多路径一次调用）、`BaiduPCS-Go ls <dir>`（单层枚举；size 列为人类可读近似值，
> 文件条目再批量 `meta` 取精确 size）。解析器：`pkg/volume/ext/baidupcs/binary_meta.go`。

| 行为 | 说明 | 锁定测试 |
|---|---|---|
| `meta` 输出字段（类型/文件大小/md5/mtime） | 解析精确字节数（忽略人类可读近似列）与 md5（标签含“可能不正确/截图请打码”变体） | `TestParseBinaryMeta_File` / `_Dir` / `TestParseBinaryMetaBlocks_Two` |
| `ls` 输出表格 | 枚举名称 + 目录标记 + mtime；size 近似值由批量 `meta` 覆盖 | `TestParseBinaryList` / `TestParseHumanSize` |
| 二进制失败回退库 adpater / 无兜底 fail-closed（不落回全量下载） | 与既有 binary→library 回退同构 | `TestBinaryAdapter_Meta_Fallback` / `_NoFallback` / `_List_Fallback` |
| 子 module e2e 测试不被 CI 编译（`//go:build e2e`）→ `make vet-e2e-submodules` 编译门 | 防外部依赖/接口变更后静默腐化 | CI `Test Sub-Modules` job 内 `make vet-e2e-submodules` |

### 7.6 百度网盘「上传后 md5/ETag 不可靠」与**秒传刷新**（实测 2026-11-04）

> **必读结论（不要重复探索）**：百度**分片上传**后远端记录的 md5/ETag **可能不可靠**
> （官方 help 原文：「分片上传之后, 服务器可能会记录到错误的文件md5, 可使用 fixmd5 命令
> 尝试修复」）。此时**内容其实已在盘**——修复手段**必须优先用 rapidupload（秒传）刷新**，
> 而**不是**重传内容：刷新零传输、毫秒级；重传要重新传整个块/文件（慢几个数量级）。
>
> **实测时间线**：把一个 130 字节文件写入**已存在的目录** `jav/_probe`，
> 逐块即时 `Stat+md5` 复核反复超时，最终 **4 分 41 秒**才完成（≈ 90s×3 重试窗口）。
> 同一时刻用 BaiduPCS-Go 直接传 1KB 到新目录，`meta` **立即可见** ——
> ⇒ 延迟**不是**「新目录/新文件的索引延迟」（此前误判），而是 **ETag 不可靠 + 刷新重试**。

| 行为 | 说明 | 锁定测试 / 实现 |
|---|---|---|
| 分片上传后 ETag 可能 ≠ 整文件 md5 | 远端 md5 由片组合而成或服务端标记「可能不正确」 | `TestDifferential_RapidUploadConvergence`（刷新命中 16/16、零重复传输） |
| **rapidupload 命中即刷新权威 md5** | 参数：`path/size/md5/sliceMD5(前256KB)/crc32/blockMD5s(4MB 切分)` | `Storage.refreshByRapidUpload` |
| 上传后复核：ETag 可用则**必须相等**；ETag 缺失（新目录窗口）则按 `sizeNear` 容差（max(4KiB,0.1%)） | 列举（`ls`）的 size 对大文件是**近似值**（实测 87.3MiB 差 312B） | `TestSizeNear` / `TestPut_StatDelayedButListVisible` |
| **秒传刷新自愈「内置」在后端**（使用方无需理解） | `BatchVerifier` 契约要求：`VerifyMany` 发现摘要不一致时**后端自己**先做 rapidupload 刷新并复验，成功即返回 `State=OK + Refreshed=true`。消费方（secretdata/上层）只看结果，**不编排**刷新逻辑 | `TestVerifyMany_SelfHealsByRapidRefresh`（变异：去掉自愈调用 ⇒ 红） |
| 批量延迟校验（上传阶段不逐块复核） | 全部写完统一校验（索引/一致性窗口后命中）；仍不一致才回落重传/整文件重试 | `TestWriteFile_BatchVerifyWholeFileRetry`（变异：去掉整文件重试 ⇒ 红） |
| 秒传刷新的**块大小边界** | `sliceMD5`/`blockMD5s` 需按 4MB 切分；块 ≤256KiB 时二者都等于整块 md5 ⇒ 可直接刷新；更大块需在 meta 存摘要列表（当前不做，回落重传） | `Storage.RapidRefresh`（`rapidRefreshMaxSize = 256KiB`） |
| 块密文摘要落 meta | `ChunkInfo.EncMD5` / `EncCRC32`（写侧由 `HashMD5`/`HashCRC32IEEE` 计算）供上述刷新使用 | `shardseal` 写侧两处构造 |

**分层判据（改造后，使用方无感）**：
1. **后端层（baidupcs）**：`VerifyMany` 内建自愈 —— 摘要不一致 ⇒ 先 rapidupload 刷新（零传输）⇒ 复验；
   成功返回 `OK + Refreshed`。这是**唯一应该实现刷新的地方**，不要在上层重复编排。
2. **卷层（secretdata）**：只看 `State`；`mismatch` 才重传（无本地副本 ⇒ 整文件重试一次）。
3. **绝不**把「ETag 不可靠」当成「内容损坏」直接判失败 —— 那正是 128 条历史任务
   16.5 分钟/条的来源（实测 130 字节文件曾耗时 4m41s）。

### 7.7 `ls -l` 可拿 MD5，但**不可用于上传关键路径**（实测 2026-11-05，结论：已回退）

**能力确认（真）**：`BaiduPCS-Go ls -l <dir>` 输出含 `MD5` 列，且对**小文件/单传**与本地
md5 一致（实测 `local = ls -l = 6156c3f7…`）；单次调用在**普通目录**约 0.3s。

**为什么最终没有采用**：按「1 次 `ls -l` 替代 N 次逐条 `meta`」改造 `Storage.list(exactSizes)`
与 `VerifyMany`（按目录分组）后，上传耗时**严重退化**：

| 文件 | 改造前 | 改造后 |
| --- | --- | --- |
| 1 MiB | 13.7s | **276s** |
| 50 MiB | 26.2s | **305s** |
| 250 MiB | 65.8s | **329s** |

回滚后同环境复测 1 MiB 仍为 **171s**，日志显示：
`WARN 上传后 Stat 等待超时仍不可见（交由上层重传）` ⇒ 说明**环境已被当晚海量测试污染**
（反复创建/删除测试目录 + 数十 GB 写入），百度侧一致性窗口变长；因此**无法把退化归因于
`ls -l` 本身**，但也**无法证明其收益**。

**处置（已落地）**：代码**回退**（`binary_meta.go` / `VerifyMany` 分组 / 解析器与测试全部移除），
保持「逐项 `Stat` + `rapidupload` 自愈」的既有路径。

**下次若要重试该优化，必须满足**：
1. **干净环境**（至少 24h 无大批量上传/删除，或换账号/新卷）；
2. **A/B 交替**（同一进程内可开关），而不是先后两次运行（环境漂移无法区分）；
3. 先只对 **`ListDir`（目录列举）** 启用（与上传路径解耦），确认 `ls -l` 在 secretdata
   **深容器目录**上不触发限流/长延迟，再考虑接入校验路径；
4. 关注 limit：历史曾因「1.2 万 blob 全量 meta」触发网盘限流 31034。

## 维护指引

- **新增外部依赖**：先在本文档对应分类补条目 + 测试锁定（TDD 红灯 → 实现 → 绿）
- **e2e 门控测试不被 CI 编译**（如 `pkg/volume/ext/baidupcs/real_network_compare_e2e_test.go` 带 `//go:build e2e`，CI 的 e2e tag 只作用于 `./test/...`）：外部依赖接口变更后，需手工 `cd pkg/volume/ext/baidupcs && GOWORK=off go vet -tags=e2e ./...` 确认仍可编译，避免测试腐化。
- **测试红时**：先对照本文档判断「依赖变化（外部改了）」vs「实现回归（我们改了）」
- **依赖变化处置**：外部行为变化 → 更新本文档 + 适配实现；实现回归 → 修实现，文档不动
- **静默变更拦截**：外部行为一旦无锁即会静默漂移（例：416 边界 / expire 签名 / keepshare 301
  / file_restore_own）——任何实现改动了上述任一行为，必须先改本文档对应行再动代码。

## 8. HTTP 直链下载的状态码与限流语义（2026-10-30）

| 行为 | 依据 | 锁定测试 |
| --- | --- | --- |
| 5xx / 429（限流）/ 408（请求超时）视为**瞬时**：下载器以 `RetryableError` 上报，管理器按 `max_retries` 重试 | RFC 9110 §15.5/§15.6（503/429/408 均为可重试的瞬时条件） | `TestStatusError_RetryClassification` |
| `Retry-After`（秒数或 HTTP-date）作为重试等待建议：优先于固定 `RetryDelay`，上限 5 分钟（防异常超长值挂死任务） | RFC 9110 §10.2.3 | `TestStatusError_RetryAfterHeader`、`TestRetryDelay_PrefersRetryAfter` |
| 其余 4xx（400/401/403/404/410）为**确定性失败**（不重试） | RFC 9110 §15.5（客户端错误不可通过重试消除） | `TestStatusError_RetryClassification` |

为何锁定：CDN 限流（429）与网关超时（408）在真实直链下载中常见；此前只重试 5xx，
一次限流即让任务直接 failed，且固定退避会持续冲击已限流的源。

### keepshare 镜像重定向（2026-11-01 观测）

- **正常**：`keepshare.org/<容器id>/magnet:?...` → 301 `keepshare.cc` → 302 `mypikpak.com/s/<id>`。
- **2026-11-01 实测异常**：对**同一 URL** 返回 `301` 指向**自身**（链长无限，永不抵达 mypikpak）。
  复现：连此前已成功解析过的分享 URL（NHDTA-360）现在也自跳。
- **影响**：`/api/resolve` 对这批分享全部 502；`--upload-local` 无法拿到文件清单 ⇒ 无法上传。
- **sproxy 侧适配**：`followKeepshareShareID` 上限 5 → **10** 跳，并新增**循环检测**
  （重复 URL 立即 `循环` 失败，不再空转跳数）。锁定测试：
  `TestFollowKeepshareShareID_LongChain`（7 跳可达）、`TestFollowKeepshareShareID_LoopDetected`。
- **判定**：属**上游行为变化/故障**，非实现回归。恢复后无需改代码（10 跳仍覆盖正常链路）。

## §7.8 BaiduPCS：移动/上传后「可见性」实测事实（2026-11-08，行为锁定）

**实测方式**：`pkg/volume/ext/baidupcs/behavior_probe{,2,3,4,5}_e2e_test.go`（`//go:build e2e`），
在**独立测试目录** `/_probe/<uniq>/` 上用真实网盘测量，收尾用 CLI `rm` 递归删除。
运行（WSL）：

```bash
cd pkg/volume/ext/baidupcs && GOTOOLCHAIN=local GOWORK=off   ~/sdk/go1.27.2/bin/go test -tags=e2e -run 'TestE2E_BehaviorProbe|TestE2E_BurstMoveProbe|TestE2E_ParentChildMoveProbe|TestE2E_ConfirmOperatorProbe|TestE2E_PutPhaseProbe' -count=1 -v -timeout 40m
```

**实测数字（同一账号/同一网盘）**：

| 操作 | 实测 |
| --- | --- |
| 单次 `move`（库 / CLI / 生产装配） | 成功，0.44s / 0.86s / 1.01s |
| **链式改名**（父→子→孙，零等待） | **8/8 成功**（0.37 / 0.40 / 0.69s） |
| **突发 50 次 move** | **50/50 成功**，0.51s/次 ⇒ **与"限流"无关** |
| 改名后确认手段 | `ls`（父列举/目标列举）**0.33–0.38s 稳定**；旧名即时消失 |
| `Meta`（库）可见性 | **偶发**：同结构一次 **≥60s 不可见**、另一次 **0.14s** ⇒ **不可作确认判据** |
| `Storage.Move` 调用数（计数 adapter） | 修复前 `Move+Meta`=2 次；**修复后 1 次** |
| `Storage.Put`（父目录已存在，库 adapter） | **90s/150s 全部 `context deadline exceeded`**；**无 Upload 入口日志** ⇒ 卡在**预检 `Stat`**（P7 归因） |

**据此确立的实现约束（已落地）**：
1. **非数据操作（move/rename）**：以 **API 返回值 + `ls` 列举**确认有效，**不得**用 `Meta` 复核
   （`Storage.Move` 已去掉 post-Stat；依据：`Meta` 偶发延迟会把成功报成 `31066`）。
2. **数据操作（上传/下载）仍必须用 `Meta`/`Stat` 取 md5 校验**（用户明确要求）：
   `Put` 的**预检**改为**有界**（`statPreCheckTimeout`，超时即放行上传），
   正确性由**上传后的 ETag/md5 复核（+ rapidupload 刷新 + size 兜底）**保证——**校验强度不变**。
3. **不得用 sleep/层间等待**规避可见性（链式零等待实测 8/8 成功）。

**错误码事实**：库错误码表 `baidupcs/pcserror/pcserrorinfo.go` 仅登记
`31045`（user not exists）/`31061`（file already exists）/`31066`（**file does not exist**）/`31079`（md5 not found）；
**未登记 `31034`**；本机日志中检索到的 `31034` 均为随机串假匹配 ⇒ 不做基于 `31034` 的限流模式匹配，
未知码**原样透传**（便于事后取证）。

### §7.8.1 上传链路阶段实测（2026-11-08，阶段打点）

`Storage` 内置 `logPhase`（Debug 级，`phase=stage|precheck|list|upload|postcheck`，
含 `elapsed_ms`/`err`/`ctx_err`，**可注入 logger**）⇒ 部署侧可直接定位"慢在哪一阶段"。

**实测 A：纯库 adapter（`libraryAdapter`，无 CLI）——小文件仍极慢且不响应 ctx**

```
phase=list      elapsed_ms=289    err=<nil>
phase=precheck  elapsed_ms=289    err=<nil>
phase=upload    elapsed_ms=89710  err=<nil>   ctx_err=context deadline exceeded   ← 卡点
phase=list      elapsed_ms=0      err="context deadline exceeded"
phase=postcheck elapsed_ms=0      err="context deadline exceeded"
```
1KB 文件上传耗时 **~89.7s**；`ctx` 超时**不能中断**该库调用（返回时 ctx 已耗尽）。

**实测 B：生产装配（binary CLI 优先 + 库兜底）——正常**

```
生产装配 Put a.bin err=<nil> 耗时=3.62s
生产装配 Put b.bin err=<nil> 耗时=3.02s
```

**结论（必须遵守）**：
1. **库上传（`libraryAdapter.Upload`）不得作为生产主路径依赖**（现状已是 binary 优先 + 库兜底）；
2. **不要给不可中断的库调用套 `context.WithTimeout`**（无效，已两次实测）；
   需要约束时改用**已实测稳定可用的操作**（如父目录列举 `ls`，0.33–0.38s）或可取消的执行方式；
3. 读路径（`Meta`/`List`）已改为**精确列举优先**（`metaByExactList`），仍取 `fd.MD5` 做
   `ETag==本地整文件 md5` 断言 ⇒ **数据校验强度不变**。

### 7.8.2 百度 v2 上传协议实测事实（2026-11-08，自研上传内核）

| 事实 | 依据 |
|---|---|
| **建会话（`uploadid`）必须先于任何上传**：rapidupload 未命中时 `return_type=1` 的响应**会带 `uploadid`**（实测 `upload_id=N1-MTgz…`）；若上游漏用/漏建会话，`superfile2/upload?uploadid=` 为空 ⇒ **HTTP 400** | 实测日志 `上传分片参数 upload_id=""` 连续 4 次 ⇒ 400；修复后同一日志非空 ⇒ 上传成功 |
| **`superfile2/upload`（`UploadTmpFile`）只写"会话临时区"，必须再 `createsuperfile` 才成为正式文件** | 实测：只上传一片不合并 ⇒ 内核报成功但远端**从未出现该文件**，既有复核 3 轮均空 ETag（284s ⇒ ErrTransient）；补 merge 后 2.87s 成功且 md5 一致 |
| **新对象列举/元信息可见性有延迟**（§7.8 同族）⇒ 上传后**立即**严格校验会假失败；权威校验应在带等待窗口的链路（`putCheckAfterUpload`，`statVisibleWait`） | 实测：上传成功后立即列举未见 ⇒ 内核 verify 假失败；改为"能看到时要求一致"后通过 |
| **小文件不省 merge**：任意大小都是「1 片 + 合并」（与库 `MultiUploader` 一致） | 同上第 2 条 |
| **会话失效（31066）⇒ 必须重建会话**：分片接口报「文件或目录不存在」= 会话临时区失效（目标正式文件尚未创建）⇒ 重走 rapid 拿**新 `uploadid`** 并**全量重传**（旧分片属旧会话）；重建**有界**（2 次） | 设计 §5；实现与锁定：`uploadcore.ErrSessionExpired` + `runPartsWithRebuild`；L1 `TestUpload_SessionExpiredRebuildsSession`、L2 `TestL2_UploadPart_SessionExpiredMapsToSentinel`（31066 三形态 + 反向对照） |
| **内核路径耗时 2.87s（1MiB）**，同环境库 `MultiUploader` 路径实测 ~90s | `TestE2E_UploadCoreNucleus` vs 早前 P7 打点 |

> 锁定方式：`TestE2E_UploadCoreNucleus`（真实网盘，nucleus 不降级 ⇒ 成功即证明内核可用；
> `BAIDUPCS_E2E_ROOT` 环境变量门控、自行清理产物）+ L1 用例
> （`TestUpload_SmallFile_OnePartPlusMerge` 断言"必须合并"；会话前置断言 `rapid==1` 且复用 `UploadID`）。

### 7.8.3 百度多分片文件的 ETag 语义（2026-11-08 真实验收；**根因已定位并修复**）

> **已修复（2026-11-08 晚）**：根因是**秒传刷新没带会话 `uploadid`**（+ 需要退避重试）——
> 修复后 8/20/120MiB 多块上传**全部收敛为权威整文件 md5**。细节与证据见下方 §7.8.3.2。
> 本节的实验数据保留为"修复前的现象"记录。

**实测（真实百度网盘，`E2E_RANDOM=1` 随机内容排除秒传）**：

| 文件大小 | 分片 | 远端 ETag | 整文件 md5 | 块列表 md5 | **回读内容 md5** |
|---|---|---|---|---|---|
| 1 / 3 MiB | 1 | == 本地 md5 ✓ | — | — | ✓ |
| 8 MiB | 3 | `0abeff4c…` | `cedc08f3…` | `d4926f49…` | **== 本地 md5 ✓** |
| 48 MiB | 13 | `7422243b…` | `51a9b06f…` | `b85e1666…` | **== 本地 md5 ✓** |

**结论（重要）**：`createsuperfile` 产物在百度侧的 **ETag 既不是整文件 md5，也不是「块 md5 列表的 md5」**
（上表三值互不相同），**但把文件取回逐字节比对，内容与本地完全一致** ⇒
**内核的多分片上传没有数据损坏**；失败纯粹来自**校验判据**：

- 内核（`uploadcore_adapter.go` 的 `Verify`）以 `meta.ETag == 整文件 md5` 判定 ⇒ 对多块文件**误报不一致**，
  且随后的秒传自愈（`Repair`）**无法改变 ETag 语义**，于是 fail-closed 报错（白报错，非损坏）。
- 分片级证据：每个分片的 `TmpFile` 返回 checksum 与本地按 4MiB 计算的块 md5 **逐一相等**（debug 日志
  `分片上传返回 … match=true`）⇒ 分片与块列表都正确。

**当前处置（护栏）**：`coreMaxVerifiedSize = uploadBlockSize (4MiB)` —— 超过时 `auto` 走既有路径、
`nucleus` 显式报错。原因是**无法用 ETag 判权威**，而非内容缺陷。

**让内核覆盖多分片的两个选项（待决策）**：
1. **校验改回读**：`Verify` 对多块文件改为「取回内容算 md5」（实测 48MiB 回读 ~1–2s、成本约一次下载）；
   代价 = 每次上传多一遍下行带宽。
2. **对齐 ETag 语义**：找出 CLI 二进制路径让 ETag == 整文件 md5 的那一步（其内部流程），在内核复刻。

> 性能对照（同环境、随机内容）：48MiB 内核 8.9s vs CLI 26–45s（120MiB）⇒ 修好校验后内核收益显著。

**复现命令**（WSL：`~/.config/BaiduPCS-Go/pcs_config.json` + 二进制在 PATH）：

```bash
cd pkg/volume/ext/baidupcs
GOWORK=off go test -tags=e2e -run TestE2E_CompareUploadPaths -count=1 -v -timeout 30m .
# 旋钮：E2E_UPLOAD_MB=48 E2E_UPLOAD_TIMEOUT=8m E2E_ARMS=core E2E_RANDOM=1
# 断言口径：硬断言 = **内容一致**（生产红线）；etagMatch 仅记录（ETag 语义见上表）
```

#### 7.8.3.2 根因定位与修复：**秒传刷新必须带会话 uploadid + 退避重试**（2026-11-08 已修复）

**根因（真实网盘隔离实验定位）**：多块文件的 `md5` 是服务端派生值，**只有经「秒传（rapidupload）刷新」**
才会变成权威整文件 md5（刷新后 `BlockList` 折叠为 `[整文件md5]`，与库 `FixMD5ByFileInfo` 的判据一致）。
而**我方两处 rapid 调用都把 `uploadid` 传成了空串** —— 库的实现传的是**当前上传会话 id**
（`internal/pcsfunctions/pcsupload/upload_task_unit.go:236` 的 `utu.state.Uploadid`）；
百度 v2 的 create/precreate 需要会话 id（早前实测缺 uploadid 直接 400）⇒ **刷新不生效** ⇒ 校验恒失败。

**修复**：
1. `ucAdapter` 在 `Merge` 时记录**会话 uploadid**，`Repair` 用它做 rapidupload 刷新
   （参数与首传同一份 `planRapid`：整文件 md5 / sliceMD5 / 随机片段 dataContent / crc32 /
   offset / dataLength / size / dataTime / block_list），并保留既有库路径作回退；
2. `uploadcore.verifyWithRepair` 改为**有界重试 + 退避**（4 次，2s/4s/8s）：远端「块 md5 索引」可见性有延迟
   ⇒ 单位 1 次刷新命中率不足（实测同一实现/同一文件有时命中有时 miss）；退避复用 `Options.Env.Sleep`
   （测试零等待）；
3. **移除尺寸护栏**（原 `coreMaxVerifiedSize=4MiB`，其前提已被证伪）：多块现在能收敛，
   安全网由 `auto` 降级（内核失败 ⇒ 本进程降级既有 CLI 路径 + WARN）承担；
4. **回归锁定**：单测 `TestUCAdapter_Repair_PassesSessionUploadID` 断言刷新**必须带会话 id**
   （变异验证：改回空串 ⇒ 用例红）——e2e 不进 CI，这条约束由单测守。

**重试预算**：`verifyWithRepair` = **6 次尝试**（退避 2/4/8/16/32s，上限 ~62s）。实测 120MiB/30 块
**4 次不够、6 次内收敛**（块数越多，远端块 md5 索引就绪越慢）；退避经 `Options.Env.Sleep` 注入（测试零等待）。

**修复后实测（真实网盘，各臂独立随机内容排除秒传）**：

| 臂 | Put | BlockList 数 | MD5 == 本地 |
|---|---|---|---|
| 内核-8MiB | ✅ | 1 | ✅ |
| 内核-20MiB（5 块） | ✅ | 1 | ✅ |
| **内核-120MiB（30 块，6 次预算内）** | ✅（`Put err=nil`） | 1 | ✅（`ETag权威=true`） |
| CLI-8/20MiB（对照） | ✅ | 1 | ✅ |

**修复前**（同一内容）：内核 8MiB `BlockList=3` / 20MiB `=5` / 120MiB `=30`，md5 全部非权威、校验 fail-closed。

**读侧 403 —— 根因已定位并修复（2026-11-08）**：刚上传的**大文件**（120MiB）经 `Storage.Get` 曾返回
**403 Forbidden**，真实网盘实验逐层收窄后确认为 **Range 块过大**：

| 实验（真实网盘） | 结果 |
|---|---|
| 手工 Range **32KiB / 1MiB / 2MiB / 4MiB** + 网盘 UA（无 cookie） | **206** ✅ |
| 手工 Range **8MiB / 15MiB**（无 cookie / 带 BDUSS cookie 皆试） | **403** `31326 user is not authorized, hitcode:104` ✗ |
| 库自己的 client（`requester.NewHTTPClient()` + 网盘 UA）发 32KiB | **206** ✅ ⇒ **客户端无责** |
| `Storage.Get`（下载器未知模式、并发 8） | 403 ✗（请求块 8–15MiB） |
| `MaxParallel=1`（仍用默认自适应模式） | 仍 403 ✗ ⇒ **与并发无关，只与块大小有关** |

**根因**：库 `downloader.Config.Mode` 默认 `RangeGenMode_Default`，其自适应生成器按「总大小 ÷ 并发」切块
（120MiB + 并发 8 ⇒ **~15MiB** 块）；百度 CDN 对非会员账号**拒绝过大的 Range**（上限即库自身
`downloader.BlockSizeList` 最大 **4MiB** ✓ 与上表实测边界吻合）。此时 `cfg.BlockSize` **无效**
（它只在 `RangeGenMode_BlockSize` 下生效）。

**修复**：`newDownloadConfig()` 显式 `Mode = transfer.RangeGenMode_BlockSize` + `BlockSize = 4MiB`
（常量 `maxDownloadBlockSize`，红线由单测守：不得超 `downloader.BlockSizeList[3]`）。
**真实场景验证**：120MiB ⇒ `内容一致=true`（修复前 false）；20MiB 亦 `true`（无回归）。

**顺带修正的另一处真实缺陷（不是本 403 的成因）**：下载器 worker 的 client 此前继承
`requester.NewHTTPClient()` 的**浏览器 UA**，而百度 CDN 对 Range 请求要求网盘 UA（缺/错 ⇒
403 `31362 sign error`，2026-10-22 已在 `syncfs_range.go` 记录同因）⇒ 现 `newDownloadHTTPClient()`
统一 `SetUserAgent(bdlib.NetdiskUA)`，且 Range **探测**（裸 `Do` 不继承 UA）显式设请求头。

**外部契约测试**（硬规则 20，`-tags=e2e`）：`TestE2E_DlinkRangeSizeBoundary` 锁定「32KiB/1MiB/4MiB ⇒ 206、
8MiB ⇒ 403」；阈值移动即红 ⇒ 先对照本节判定是依赖变化还是回归。

**已排除的旁路假设**（保留以免重复探索）：
- 调库的 `BaiduPCS.FixMD5`（`BaiduPCS-Go fixmd5` 同实现）**修不了**——返回 `err=nil` 但 ETag 在 ~42s 内完全不变；
- **不是块大小问题**：把分块临时改 16MiB（库默认）后多块仍失败，只有单块成功。

> 复现/回归：`pkg/volume/ext/baidupcs/fixmd5_probe_e2e_test.go`（`-tags=e2e`，CI 不跑）与
> `TestE2E_CompareUploadPaths`（`E2E_UPLOAD_MB`/`E2E_ARMS`/`E2E_RANDOM` 旋钮）；
> 单测锁定见 `l2_contract_test.go` 的 `TestUCAdapter_Repair_PassesSessionUploadID`。

#### 7.8.4 负面结论：`httptest` 直驱 fork 库**不可行**（2026-11-09 两次实测，避免重复探索）

**目标**：用 `httptest` 假百度 + 自定义 `RoundTripper` 重写 host，在 CI 里锁定「库的 HTTP 行为 +
我们的应对」（错误码、请求形状）。

**实测（两种注入方式都失败）**：
1. `NewPCSWithClient(266719, hc)` + `hc.Client.Transport = rewriteTransport`，并注入 cookie jar 的
   `BDUSS`/`STOKEN` ⇒ **一个请求都没发出**，库本地报 `31045 user not exists`；
2. 改用**库实际持有的** client（`pcs.GetClient()`）替换其 `Transport` ⇒ `CacheUK()` 仍报错，
   `rewriteTransport` **记录为空**（请求未经过我们注入的 Transport）。

**结论**：障碍在**库的内部 client 管理**（`GetClient()` 返回的实例未必是请求实际使用的那个），
**不值得为测试反向适配生产代码**。⇒ 本仓的「库 + HTTP 协议」层锁定改走 **窄接口注入**
（`ucPCS`：`CacheUK`/`RapidUpload`/`FakeRapidUpload`/`UploadTmpFile`/`CreateSuperFile`），
见 `pkg/volume/ext/baidupcs/l2_contract_test.go` —— 无网络、更稳。

> 订正记录：早前曾把现象记为「库在发 HTTP 前做会话/uk 前置检查」；实测表明请求**确实发生**，
> 但**未走我们注入的 Transport**。勿再据旧结论设计 httptest 方案。

#### 7.8.3.1 可观测：内核"在不在跑、为何没跑"（2026-11-08 落地）

> 注：`skipped_oversize`（废弃口径，原尺寸护栏已随 §7.8.3.2 的修复移除、恒 0）**已于 2026-11-09 删除**
> （含 `UploadCoreStats` 字段、`UploadCoreStatsCounts` 签名与 `/metrics` 的 `mode` 枚举，1 处一起改完，
> 避免误导运维。

**领域层计数**（`pkg/volume/ext/baidupcs`，原子计数，随时可读）：

| API | 含义 |
|---|---|
| `Storage.UploadCoreStats()` | `UploadCoreStats{Used, Degraded, Failed, LegacyMode, NoDeps}` |
| `Storage.CoreDisabled()` | 本进程是否已因内核失败降级（auto） |

**日志**（结构化字段，便于日志检索/告警）：
- 首次跳过（超尺寸护栏）：`msg="上传内核跳过（超过尺寸护栏）——本次起该尺寸走既有路径" key=… size=… limit=…`（每进程仅一次，避免逐文件刷屏）；
- 内核失败降级：`msg="上传内核失败，本进程降级为既有路径（auto）" key=… err=…`（WARN）。

**已接入 `/metrics`（抓取时探测，快照语义）**：

```
sproxy_volume_upload_core_total{volume="<卷>",mode="used|degraded|failed|legacy_mode|no_deps"} N
```

链路：`/metrics` 渲染时的卷遍历（`renderVolumeWatermarks`）→ `volSet.External(name)` **能力探测**
（`pkg/server` 的 `uploadCoreCountsProvider`，只收原始整数 ⇒ 指标层不依赖具体后端类型）→
`cmd/sproxy` 的 backend 包装器**转发**（`UploadCoreStatsCounts()`，把 `baidupcs.Storage.UploadCoreStats()`
的结构体适配成 6 元组；nil 来源返回全 0）→ `metrics.SetVolumeUploadCore`（**覆盖式**写入，避免每次抓取重复累加）。

注意：**透明装饰器必须转发能力**（R21 同族）——包装器漏转发 ⇒ 探测恒 false ⇒ 生产看不到"内核在不在跑"，
故 `cmd/sproxy` 侧有专门用例锁定该转发与新式接口形状。

