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

## 3. 未修（记档，需决策或属可观测性）

| 项 | 级别 | 说明 / 为何未在本批 |
| --- | --- | --- |
| secretdata 目录改名时旧 dir meta 删除失败 ⇒ 容器内可能并存两个 dir meta | P1 | 需先定**加载期仲裁口径**（哪个 meta 权威：物理位置 / DirID / 版本），单改「加日志」不解决静默回退；属设计决策 |
| secretdata 锁内远端 `Delete`/`ListDir` 未统一有界（`removeEmptyContainerLocked`、GC 两处） | P2 | 只在「远端/阻塞型底层」可达；与既有 `boundedCleanupCtx` 口径统一即可，随 P1 一并做 |
| secretdata `Delete` 的 meta 物理删失败完全静默且进程内不可重试（GC 又保护它） | P2 | 既有用例把「返回成功 + 重启重建」钉为**有意方向**，改的是可观测性/重试语义，需与产品确认 |
| secretdata 读句柄的「EOF 整文件哈希」在 `Seek` 消费方（含 `http.ServeContent`）恒失效 | P2 | 有逐 blocklet GCM + `meta.VerifyReadSeeker` 兜底，属纵深校验缺失 |
| 百度网盘 binary-only 模式下「CLI 执行失败」被判成「路径不存在」 | P2 | 错误分类改进（`exec.ErrNotFound` 优先） |
| 百度网盘 `List` 的精确 size 回填失败时用 `ls` 近似值当权威 | P2[推测] | 会在每轮同步反复重传；需真实网盘验证路径键一致性 |
| 组归档转存外部卷的产物被删除时不释放 owner 配额（≤30min 被磁盘对账冲掉） | P2 | 有界影响 |
| 审计 `.archiving.<n>` 隔离段在归档中途崩溃后不可见/不重试/不计入 `max_total_bytes` | P2 | 启动期扫描改名回原名即可，随下批做 |
| `MarkDone` 之后仍可 `Emit`（末条不再是 KindDone ⇒ 作用域永远未完成） | P2 | 需使用方违约才触发；与 `Scope.Save` 的既有 `st.done` 检查口径不一致 |
| WAL-only 能力集下 `MarkDone` 静默 no-op | P2 | 当前无预设触发（`CapabilitySet` 四预设都不含该组合） |
| 启动恢复的 5 分钟预算对 legacy cloud 注册项无效（`RecoverAll` 无 ctx） | P2 | 需产品确认「恢复完成前不对外服务」是否必须 |
