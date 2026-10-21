# 生产稳定性对抗审查（R15）——secretdata / 百度网盘 / 转存 / 审计恢复

- **基线**：master `8548e471b`（PR #840 合并后）
- **方法**：四区域并行 **fresh-context 只读审查**（每区域一个 reviewer，独立上下文），
  每条结论要求「文件:行号 + 触发条件 + 后果 + 现有测试为何挡不住 + 自我证伪」；
  修复遵循 TDD（先红）+ **变异命中**。
- **维度**：正确性 / 可用性（恢复与错误语义）/ 安全性 / 可维护性（可观测性）
- **结论**：修复前 **4 个 P0、13 个 P1**；本批修复 **2 个 P0 + 9 个 P1**（11 项，全部变异命中）。
  剩余 P1/P2 记档于 §3（需产品/架构决策或属于可观测性改进）。

## 1. 已修（本批）

### P0-A secretdata：卷锁自锁死（整卷永久挂起）
- **位置**：`pkg/volume/secretdata/secretdata.go` `rollbackWrite → pruneCreatedDirs → s.mu.Lock()`。
- **形态**：`pruneCreatedDirs` **无条件**取写锁；而 `rollbackWrite` 的 4 个「无新建目录」调用方
  （`writeRenamedMetaLocked`/`writeDirMetaLocked`/`copyContentBlobs.rollback`/`CopyFile`）
  **正持有 s.mu**（改名/复制路径）⇒ `sync.Mutex` 不可重入 ⇒ 死锁；此后该卷所有读写永久阻塞，
  只能重启进程，用户侧表现为 `mv`/`cp` **无响应**（不是报错）。
- **现有测试为何挡不住**：`rename_crash_window_test.go` 的注入是「操作**成功后** panic」——panic
  路径不执行回滚代码；`faultWriteFS` 只用于**非持锁**的主写路径。
- **修复**：`len(created)==0` 早退（语义等价，锁内调用方全部传 nil）。
- **测试**：`TestRenameMetaWriteFailure_DoesNotDeadlockVolume`（10s 超时断言 + 修复后卷仍可服务）；
  变异命中：去掉早退 ⇒ 用例以「10s 未返回」变红。

### P0-B secretdata：删目录最后一个文件会删掉同目录「在途写」已上传的分块
- **位置**：`pruneEmptyDirsLocked` / `dirHasContentLocked` / `removeEmptyContainerLocked`。
- **形态**：写路径在 `ensureContainer` 与 `commitLocked` 之间**不持锁**上传；`dirHasContentLocked`
  只看已提交索引（不含 `writesInFlight`）⇒ 并发删除最后一个已提交文件时判「空目录」⇒
  `removeEmptyContainerLocked` **清空容器内全部文件**（含在途写的分块/meta）⇒ 写入仍返回成功
  （幽灵条目），底层若重建目录则容器**无目录 meta** ⇒ 重启时该容器整段 fail-closed 跳过 ⇒
  **已确认成功的上传永久消失**。窗口 = 整个上传时长（远端可达分钟级）。
- **现有测试为何挡不住**：并发压力用例的写/删目录**不相交**；单线程回收用例无法产生交错。
- **修复**：`pruneEmptyDirsLocked` 在 `writesInFlight > 0` 时直接返回（与 GC 的既有门禁同判据；
  代价只是空容器延后回收）。
- **测试**：`TestDeleteLastFile_KeepsInFlightWrite`（阻塞式 inner FS 制造交错 + 重启重挂载断言）；
  变异命中：去掉门禁 ⇒ 「重启后文件丢失」变红。

### P1-C 审计 `ReadTail` 段序写反 ⇒ `/api/audit?limit=N` 返回**最旧** N 条
- `archiveIndex` 越大越旧（`shiftArchives` 把 `.1`→`.2`），而排序用了**降序** ⇒ 先读最旧段、
  凑满 limit 就 break；`skippable=true` 还会连带跳过冷段。热目录存在封段时（`max_size>0` 且未配
  归档）审计面板静默显示很久以前的记录，**当前事件不可见**（合规取证直接错）。
- 修复：改**升序**（活跃段 0 最前）。
- 测试：`TestFileJournal_ReadTail_ReturnsNewestAmongSealedSegments`（每写一条即轮转 ⇒ 多封段共存）；
  变异命中：改回降序 ⇒ 返回 `act-00..02` 变红。

### P1-D 冷段「末条时间探测失败」被伪造成首条时间 ⇒ 删掉保留期内的证据
- `probeSegmentTimes` 的 `last = first` 回退让 `PurgeCold`/retention 的 `Last.IsZero()` 保守守卫
  **永久失效**：一个「首条 8 天前、尾部 1 小时前」的段会被整段判过期删除；`since=` 查询亦整段误跳。
- 修复：未知即零值（让保守分支生效）。
- 测试：`TestProbeSegmentTimes_LastUnknownStaysUnknown`（只让「非零起点区间读」失败的 FS，加
  `PurgeCold` 端到端断言）；变异命中：恢复 `last=first` ⇒ 段被删变红。

### P1-E `seqFloor` 全量解析 1GiB 段 ⇒ 重启后首次审计写入 OOM
- 生产合规段族是**单文件全局段且默认不轮转**（上限 `max_total_bytes` 默认 1 GiB）⇒ `Read` 全量
  解析数百万条带 `Meta` 的 Entry，峰值 GB 级并同步阻塞该请求。
- 修复：`journalMaxSeq`/`maxSeqInSegmentFile` —— 从**最新段**反向按行扫描（256 KiB 块 + 跨块
  carry），命中该作用域第一条即返回；内存与段大小无关；`MemoryJournal` 走线性扫描；第三方
  Journal 实现回退旧语义。
- 测试：`TestMaxSeqInSegmentFile_CrossChunk`（>256KiB、最大 Seq 不在末条）+ 既有
  `TestSeqContinuesAcrossProcessRestart`；变异命中：`journalMaxSeq` 恒返回 0 ⇒ 续号用例变红。
- **自查发现**：首版实现把「文件首行」在逆序扫描**之前**处理 ⇒ 返回最旧 Seq；**既有**用例
  `TestSeqContinuesAcrossProcessRestart` 立刻抓到（说明既有测试有效）。

### P1-F 云端孤儿组重建：同组 N 个任务只保留 1 个成员
- `recoverOrphanGroups` 在 `m.groups` 尚未填充时逐任务建组（`TaskIDs:[t.ID]`、`TotalTasks:1`），
  map 最终只留最后一个 ⇒ 组视图丢成员、**单个成员完成即判整组 completed**、组级 cancel/archive
  只作用于 1 个任务（其余继续占并发与配额）；`cleanupExpiredGroups` 还会把整组删掉。
- 可达路径：`CreateGroup` 先建子任务、最后才 `saveGroup` 的窗口崩溃/磁盘满；组文件损坏被跳过。
- 修复：按 GroupID 聚合一次（`mergeOrphanTask`），成员排序保证可重现；读 `m.groups` 统一走 `groupMu`。
- 测试：`TestRecoverOrphanGroups_AggregatesSameGroup`；变异命中：退回 `TaskIDs=[t.ID]` ⇒ 红。

### P1-G 转存首写中途失败留下半截对象 ⇒ 重试永久「拒绝覆写」+ 永久孤儿
- 非原子写的目标卷（FTP STOR / WebDAV PUT / trusted 降级）写途中断会留下半截对象；而
  `writeCount` 只在写成功后 ++ ⇒ 重试仍走「目标唯一」分支 ⇒ `Stat` 命中自己的残骸 ⇒ 每次重试
  报「已存在（拒绝覆写）」⇒ 退避耗尽后 failed（文案误指目标卷异常），残骸永久留存并已计入账本。
- 修复：在「Stat 已确认不存在」的分支里，`WriteFile` 失败后立即 `Delete` 清理（有界 ctx，
  不触碰他人对象 ⇒ 唯一性语义不变），与 `pkg/transfer` 的 `writeUnique` 同纪律。
- 测试：`TestWriteTargetUnique_FailedWriteCleansPartial`（写半截后失败 ⇒ 断言无残骸 + 恢复后重试成功）；
  变异命中：去掉清理 ⇒ 重试仍被「已存在」阻断变红。

### P1-H 云端归档（落桶）checksum 恒错 + 收尾错误被吞（截断归档当成功）
- `createTarGz`/`createMultiFileTarGz` 用 `defer tw.Close()/gw.Close()`，而 checksum 在 defer
  **之前**计算 ⇒ 哈希漏掉 tar 的 EOF 两块 + gzip 尾（CRC32/ISIZE）⇒ 响应里的 checksum **100%**
  与落盘文件不符（客户端校验必然判「归档损坏」）；Close 错误丢弃且 `succeeded` 已置 true ⇒
  收尾失败的**截断归档**被当成功返回且不清理。
- 修复：显式顺序关闭（tar → gzip → file）并收集首个错误，**关闭后**再算 checksum
  （与 `pkg/server/archive.go`、`pkg/transfer/registry.go` 既有口径统一）。
- 测试：`TestCreateTarGz_ChecksumMatchesArtifact`（比对落盘 SHA-256 + gzip/tar 完整解压）；
  变异命中：在关闭前算 checksum ⇒ 红。

### P1-I 百度网盘库兜底下载：把 206 的 `Content-Length`（区间长度）当文件总大小
- 探测请求 `Range: bytes=0-1023` 拿到 206，其 `Content-Length` 是**区间长度**（1024）⇒
  并行下载器用 `Content-Range` 的真实 total 比对 ⇒ `total length is unexpected` ⇒ **任何 >1 KiB
  文件的读取恒失败**；若中间层不回 `Content-Range`，则只写 1 KiB 就判成功 ⇒ **静默截断**。
- 修复：优先级 `x-bs-file-size` → `Content-Range` 的 total → 仅 **200** 才用 `Content-Length`；
  探测区间与上游对齐（32 KiB）。
- 测试：`TestProbeTotalSize`（5 种响应形态）+ `TestBaiduDURLCheck_Wiring`（真实 HTTP 206 接线）；
  变异命中：接线改回 `resp.ContentLength` ⇒ 探测值 1024 变红（**只测 `probeTotalSize` 会漏接线**）。

### P1-J 百度网盘分片上传：持续性分片错误无终止判据 ⇒ 无限重试 + 整卷上传锁永久占用
- 上游 `MultiUploader` 只认 `*uploader.MultiError{Terminated:true}`，其余错误**零退避、无次数上限**
  重排；本包 `TmpFile` 只 `%w` 包 pcserror ⇒ 403 风控 / uploadid 失效 / 413 会让 `Execute`
  **永不返回**（热循环轰 API）：sync 任务永久 `syncing` 并占满并发槽，`libraryAdapter.uploadMu`
  （整卷一把）被永久持有 ⇒ 该卷后续所有上传阻塞。
- 修复：`shouldTerminatePart` —— HTTP 400/401/403/413 立即 Terminated（与上游 CLI 同构）+
  每分片 6 次上限（`maxPartAttempts`，堵 5xx/网络死循环）。
- 测试：`TestShouldTerminatePart`（4xx 首击终止 / 5xx 次数上限 / 分片计数独立）；
  变异命中：禁用两条终止判据 ⇒ 红。

### P1-K 百度网盘库兜底下载不受 ctx 约束 + 失败残留等大 `.part`
- `ctx` 只包探测请求，`Execute` 无 ctx、`Cancel` 从未调用 ⇒ 取消/超时/停机后仍把**整个文件**下完；
  且失败路径从不删 `<file>.part`（持久目录下 GB 级泄漏，全仓无清扫器）。
- 修复：`<-ctx.Done(); der.Cancel()`（与上传侧同构）+ 失败 `defer os.Remove(finalTmp)`。

## 2. 验证

| 范围 | 结果 |
| --- | --- |
| 根 module `go test ./...`（workspace，96 包） | 全绿（含 `internal/archcheck`） |
| `pkg/volume/ext/baidupcs`（独立 module） | 全绿 |
| e2e `go test -tags=e2e ./test/...`（3 包，含 19 个恢复用例） | 全绿 |
| `golangci-lint run` | 0 issues |
| 变异验证 | 11/11 命中（本批全部修复） |

## 3. 第二批修复（本 PR 同一分支内跟进，2026-10-21 晚）

上一版列在「未修」的条目已按如下口径全部落地（仍全部 TDD + 变异命中）：

| 原「未修」项 | 处理 |
| --- | --- |
| secretdata 目录改名旧 meta 删除失败 ⇒ 容器内并存两个 dir meta，重启可能**静默回退/翻覆** | **确定性仲裁**：`dirMeta.Version` 在改名/移动时单调 +1（权威信号），仲裁键 = Version → mtime → 名字。为何不用 mtime 做主键：`ListDir` 的 `Entry.MTime` 在本卷实现上恒 0，且真实文件系统时间精度可能相等 ⇒ 退化为按**随机盐文件名**排序（实测约 50% 概率回退旧名，用例连跑 6 次复现）。旧 meta 的删除改为 `boundedCleanupCtx` + **Warn 可见**。用例：`TestRenameDir_StaleDirMeta_DeterministicArbitration`（变异：去掉 `Version++` ⇒ 红）。 |
| secretdata 锁内远端 `Delete`/`ListDir` 未统一有界 | `removeEmptyContainerLocked` 自带 `boundedCleanupCtx`；GC 的锁内删除收敛为 `deleteKillsLocked`（同一有界 ctx）。 |
| secretdata `Delete` 的 meta 物理删失败完全静默 | 增加 Warn（保留「先删 meta + 分块全保留」的安全失败方向不变：重启重建完整文件）。 |
| secretdata 读句柄「EOF 整文件哈希」在 `Seek` 消费方恒失效 | 语义收紧为「**已读过数据后再跳转**才失效」：`http.ServeContent` 的 `Seek(End)→Seek(Start)` 不再关闭校验；读前位置 ≠ 0（真随机读）由 `Read` 侧判据关闭。用例：`TestReadSeeker_SeekToSamePositionKeepsEOFVerify`（含反证：错误哈希必须在 EOF 报错）。 |
| 百度网盘 binary-only 模式下「CLI 执行失败」被判成「路径不存在」 | `isExecUnavailable` 优先分类：**有库兜底仍用库**（保持原设计），无兜底则如实上抛执行失败（绝不 `ErrNotFound`）。用例：`TestBinaryMeta_ExecutorUnavailableNotNotFound`。 |
| 组归档转存外部卷的产物被删除时不释放 owner 配额 | 删除前用**同一** `archiveTargetCharge` 实测、删除成功后按同一 Scope `ReleaseUsage`。用例：`TestDeleteGroupArchive_ReleasesOwnerQuota`（真实 HTTP 归档 → 删除 → 账本下降）。 |
| 审计 `.archiving.<n>` 隔离段在归档中途崩溃后不可见/不重试/不计入上限 | 启动期 `recoverQuarantinedSegments` 把隔离段改名回原名（原名被占用则取「最大序号 + 1」），Warn 可见。用例：`TestNewFileJournal_RecoversQuarantinedSegment`。 |
| `MarkDone` 之后仍可写（末条不再是 KindDone ⇒ 作用域永远未完成） | `prepare` 对 `st.done` 的非提交类条目返回 `ErrScopeClosed`（与 `Scope.Save` 口径统一）。用例：`TestService_NoWritesAfterMarkDone`。 |
| WAL-only 能力集下 `MarkDone` 静默 no-op | 返回 `ErrCapabilityUnavailable` 并回滚 `done`（可重试）。用例：`TestMarkDone_WALWithoutComplianceNotSilent`。 |
| 启动恢复 5 分钟预算对 legacy cloud 注册项无效 | `RecoverAll(ctx)` / `recoverTasksFromDir(ctx, …)` 全链路下传 ctx，恢复循环逐条目检查取消；装配层闭包不再丢弃 ctx。用例：`TestRecoverAll_CanceledContextStopsRecovery`（变异：去掉 ctx 检查 ⇒ 红）。 |

### 仍开（需真实环境/产品口径，非代码缺口）

| 项 | 说明 |
| --- | --- |
| 百度网盘 `List` 的「精确 size 回填失败 ⇒ 用 `ls` 近似值当权威」 | 因果链成立但**触发条件未在真实网盘证实**（键匹配是否必然一致）；修法（回填失败置 Size=0 或标注未知）需先确定 `pkg/sync/diff` 对「未知 size」的语义，避免引入每轮重传的另一种形态。 |
| presign 直传纳入 meta + 容量记账；回收站多卷配额释放；加密卷容量精确记账；secretdata 同卷复制优化 | 已在 roadmap 记为产品口径项（非缺陷）。 |

### 已知 flake（与本 PR 无关，记录备查）

- `pkg/sync` `TestEngineSync_ContextCancelMidSync`（全量并发跑时偶发，单跑/整包重跑均绿）。
- `pkg/cloud` `TestCloudDownloadManager_TimeoutThenSuccess`（同上）。
- `pkg/tunnel/hub` 并发 sync 用例、`pkg/server` `TestTierWarm_PromoteWarmOnRead` / `TestS3Complete_OverLimitBody_413`。
- UI E2E `TestAuth_SaveKeysSigns`（本次 PR 首次跑红，`gh run rerun --failed` 后绿；仓库 0 个 web 文件改动）。

## 4. 验证（第二批后）

| 范围 | 结果 |
| --- | --- |
| 根 module `go test ./...`（96 包） | 全绿（`pkg/sync` 一处已知 flake，单跑绿） |
| `pkg/volume/ext/baidupcs`（独立 module） | 全绿 |
| e2e `-tags=e2e ./test/...` | 3 包全绿（含 19 个恢复用例） |
| `golangci-lint run` | 0 issues |
| 变异验证（第二批 8 项） | 8/8 命中 |

## 5. 第三批：用户实测问题 + 同类型普查（2026-10-21 深夜）

用户报告「Web UI『过滤卷』下拉看不到运行时创建的 `vault`」。定位为**前端派生状态不刷新**
（非服务端数据问题：`GET /api/volumes` 返回齐全），并**普查同类**一并修复：

| 缺陷 | 触发 | 修复 |
| --- | --- | --- |
| **「过滤卷」/「上传卷」下拉不刷新** | 两下拉只在 `DOMContentLoaded` 初始化一次；登录前 `/api/volumes` 401 ⇒ 登录后不再重试 ⇒ 运行时建的卷不可见 | `refreshAfterAuth()` 统一入口（两个下拉 + 事件流 + 断点续传探测 + 打开着的监控 tab + 列表），登录成功（`login.js`）与**手动保存 AK/SK**（`saveAccessKeys`）都调用 |
| **事件流（SSE）401 后永久停止** | 旧实现 401 直接 `return`，不留标记也不重连 ⇒ 登录后实时更新仍死 | 记 `_eventsAuthBlocked` 暂停无凭据空转重连；`eventsRestartAfterAuth()` **关闭旧流并重连**（身份变了 ⇒ 服务端 owner 过滤也不同） |
| **断点续传列表不刷新** | `checkResumableUploads()` 只在加载时探测，登录前探测失败静默隐藏 | 纳入 `refreshAfterAuth` |
| **监控弹窗开着时内容不刷新** | 登录前打开任意 tab 会渲染失败占位，且不重试 | `refreshAfterAuth` 检测弹窗打开态 → `statsRefresh()` 重拉**当前活动 tab** |
| **config 静态声明 `type: baidupcs` 等外部卷启动失败** | 后端类型注册在 `RegisterRoutes`（装配）**之后** ⇒ `assembleVolumes → registry.NewBackend` 报「未注册后端」；只能运行期建卷绕过 | 新增 `registerVolumeBackendTypes()` 在 `setupServerCore` **最早**调用（类型与建卷 schema 全前移）；sync 侧只保留需要 `syncexec` 的 FS 工厂接线。门禁 **R45** 钉「注册先于装配」+「sync 阶段不得回退注册」 |

**验证**：
- 真浏览器 e2e `TestVolumes_LoginRefreshesSelects`（先把登录前 `/api/volumes` 路由拦截成 401，
  再走真实注册 + TOTP 登录，断言两个下拉被填充 **且**出现新的 `/api/events` 连接）；
  变异命中：① 登录后只 `refreshList()` ⇒ 红；② 不关闭旧事件流直接复用 ⇒ 红；
- `TestRegisterVolumeBackendTypes_EnablesExternalVolumeAssembly`（生产注册函数 + `registry.NewBackend`
  覆盖 webdav/sftp/ftp/s3/baidupcs；sftp/ftp 构造期拨号，判据为「错误来自该后端自身」而非「未注册」）；
- 门禁 R45 变异命中：删掉 `registerVolumeBackendTypes()` 调用 ⇒ 红。

**仍未做（设计级，用户已同意留后续片）**：secretdata **外部卷**重启后遗留「不可解容器」目录
（loadIndex 清空索引后对遗留根容器既不清扫也不挂载）——需要先定语义（死亡目录 GC vs 仅索引不挂载，
影响外部卷写放大与删除语义），不在本批范围内。

## 6. 第四批：加密卷/外部卷「按路径寻址」场景普查（用户实测：分享返回 404）

### 6.1 缺陷与修复
用户实测：`vault`（`type: secretdata` over 百度网盘）中的文件创建分享 → **404**。
根因：分享创建/访问只经 `locateOwnerFile`（**本地卷租户根**）定位源文件 ⇒ 外部卷（含加密卷）
恒不命中。**同类普查**（凡「按 owner+rel 找源文件」的实现）后统一收敛为
`pkg/server/source_volume.go` 的 **`resolveOwnerSource`**（本地租户根 / 外部卷 FS 二选一），
并逐个场景修复：

| 场景 | 修复前 | 修复后 |
| --- | --- | --- |
| **分享创建**（`POST /api/share`） | 只认本地卷 ⇒ 外部/加密卷 404 | 新增可选 `volume`（请求体/查询参数）；缺省自动定位（本地卷优先，未命中再扫外部卷） |
| **分享记录** | 只有 `TenantID/Rel` | 新增并持久化 `Volume`（**固化**分享源卷：多卷同 rel 语义确定；旧记录空值走兼容路径） |
| **分享访问**（`GET /s/<token>`） | 按 rel 重新扫描本地卷 | 有 `Volume` ⇒ **只在该卷**重新定位（ACL 复核 + 存在性）；外部卷经卷 FS 透明解密返回**明文**，`Content-Length` 用明文长度 |
| **AI 摘要/标签**（`/api/ai/summarize|tag`） | `tenantFor(owner)` 默认租户根 ⇒ 外部卷「文件不存在」 | `resolveOwnerSource` + 可选 `?volume=`；缓存键带卷名（防跨卷同名命中） |
| **版本列表**「文件是否可见」兜底 | `locateOwnerFile`（本地）⇒ 外部卷文件 404 | `resolveOwnerSource`（卷感知） |
| **服务端归档**（`POST /api/archive`） | 外部卷文件走到本地根后报「stat 失败」（误导），或在默认卷被 ACL 排除时**静默缺项** | 明确失败并说明原因：`文件位于外部卷（加密卷/网盘卷/S3），服务端归档暂不支持；请直接下载`（**不静默缺项**） |

已核实**本来就支持**外部/加密卷的场景（未改动）：`/download`、`/download/chunk`、`/api/files?volume=`、
`/api/du`、回收站（各卷独立 + 汇总）、DAV（按卷挂载）、S3（按桶）、sync（按远端卷）、
转存/归档转存（`TransferFSFor`）、备份、`/api/volumes/{move,copy}`、预签名（按卷）。

### 6.2 仍不支持（明确报错，非静默）
- **服务端归档**（`/api/archive` 打包为桶内 tar.gz）只支持本地卷（实现基于 `os.Root` 的符号链接
  安全 + TOCTOU 校验）；外部卷文件现在**明确报错**，不再静默缺项。若要支持，需要一套外部 FS 的
  等价遍历/校验实现（后续片）。
- **版本恢复**把历史版本写回当前文件：写面只经本地卷根（外部卷无版本桶），保持既有语义。

### 6.3 测试
`pkg/server/share_external_volume_test.go`（真实 `secretdata` 外部卷，含加解密）：
分享「显式卷 / 自动定位 / 多卷同名以显式卷为准 / 源删除 410」+ AI 抽样 + 归档显式失败；
前端 `sclient.test.js` 覆盖 `volume` 透传；Web UI「过滤卷」选中时分享请求自动带上卷名。

## 7. 第五批：服务端能力 → **客户端接入**核查（CLI / Web UI）

修复能力若客户端不接入 = 用户仍不可用。逐项核查后补齐：

| 服务端能力 | CLI | Web UI |
| --- | --- | --- |
| `POST /api/share` 新增 `volume`（外部/加密卷分享） | ✅ `share create <卷名>:/<路径>`（卷 URI，与 `ls/du/download` 同规约）+ `cd` 工作目录卷；`share list` 增 **VOLUME 列**、`share create` 输出显示「源卷」 | ✅ 文件行「分享」按钮带 `data-volume`（行卷）→ 弹窗显示**源卷**提示 → 创建请求带 `volume`；分享列表行显示卷徽标 |
| `GET /api/shares` 返回 `volume` | ✅（见上） | ✅ 列表行卷徽标 |
| `/api/trash?volume=` / `restore?volume=` / `empty?volume=|all=1`（多卷回收站） | ✅ `trash list [--volume]`（含 VOLUME 列）、`trash restore --volume`、`trash empty [--volume|--all]`（**安全默认只清默认卷**，`--all` 与 `--volume` 互斥） | ✅（已在 #830 接入：卷筛选 + 卷列） |
| `/api/ai/summarize|tag` 新增 `?volume=` | —（CLI 无 AI 命令） | —（Web UI 未接入 AI 面板；后端能力留待 UI 功能化） |
| `/api/archive` 对**外部卷**文件显式报错 | ✅ 错误直传（文案已说明原因） | ✅ 同上 |

**核查结论（本来就已接入，未改动）**：`/download`、`/download/chunk`、`/api/files?volume=`、
`/api/du`、DAV/S3、`sync`、云端转存/归档转存（`target_volume`）、`volume move/copy`、
预签名、`trash`（Web UI）、审计卷维度筛选（Web UI）。

**测试**：
- CLI：`TestShareCreate_VolumeURI`（卷 URI 剥离 + 请求体 `volume` + 输出源卷）、
  `TestTrash_MultiVolume`（`--volume`/`--all`/默认不扩大/互斥）+ 既有 share/trash 集成用例；
- Web UI：`share_version_e2e_test.go` 扩展——行「分享」→ 弹窗**源卷提示** → 捕获 `POST /api/share`
  断言 `volume=default`（真浏览器，缺接线必红）；
- 纯函数：`app-render.test.js` 的 `shareVolumeFor`（行卷优先 → 过滤卷 → 空）与分享按钮 `data-volume`；
- 文档：`docs/cli.md` 补 `share` / `trash` 两节（含卷寻址与安全默认）。
