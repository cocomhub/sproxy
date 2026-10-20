# 生产可用性对抗评审（R14）——2026-10-21

> 方法：**两轮、fresh-context、只读子代理**，互不可见。
> 第一轮 5 个视角（恢复/重试 · 配额等待 · 新代码 · 跨切可用性 · 质量性能）；
> 第二轮 3 个**证伪者**逐条尝试推翻第一轮的 14 条主张（默认「原报告是错的/夸大的」）。
> 基线：`master 49c9910ae`。结论以**证伪后的裁决**为准，未证伪项单列。

## 0. 总判定

**当前不能宣称「生产级可用」**，四条硬保证的实际状态：

| 保证 | 状态 | 说明 |
| --- | --- | --- |
| 下载/转存/上传**中断后自动重试** | ⚠️ 部分 | 网络瞬断有有界重试与 `.partial`+Range 续传；但**下载完成→转存**窗口重启会**整文件重下**（C2） |
| **服务重启自动继续** | ⚠️ 部分 | 单条 `downloading` 能续；**排队中（pending）任务一律转 failed**，批量/组任务大面积不续，1h 后被清理（C1） |
| **配额不足自动等待重试** | ❌ 不一致 | owner Scope 会等；**卷容量池上传路径不等待**（C4）；**归档转存完全没有写前配额探针**（C6）⇒ 配额满仍写入越界 |
| **数据不丢** | ⚠️ 需人工 | 删除回滚/回收站 meta 迁移失败**静默**（C8/C9），文件变 `.deleting.*` 需人工改名；软删失败**不回写**（新发现） |

另有三类「非功能但会打穿上表」的缺陷：写锁 30 分钟被回收导致**互斥失效**（C7）、加密卷 Range 下载 **O(offset) 解密**（C10，1 GiB 分块下载 ≈ 64 GiB 解密）、审计读面边界丢条目（C11）。

## 1. 已确认（CONFIRMED）——按修复优先级

| # | 级别 | 缺陷 | 关键证据 | 最小修法 |
| --- | --- | --- | --- | --- |
| C1 | **P1** | 排队任务重启后不续：磁盘状态仅在取得信号量后写 `downloading` | `pkg/cloud/manager_task.go:892-896`；`manager_persist.go:245`（只重启 downloading）、`:284-291`（pending→failed）、`manager.go:230-231`（FailedTaskTTL 1h） | Submit 后即落盘 `queued`；恢复时把「无 goroutine 的 queued」重新提交（`failOrphanPendingTask` 只处理「从未 Submit」） |
| C2 | **P1** | 「下载完成、正在转存」窗口重启后**整文件重下**：无 `transferring` 状态、无「dest 已完整」短路 | 全仓无 `transferring`；`manager_task.go:1149` 状态一路到 completed；`http_downloader.go:242-248` 仅 `.partial` 存在才续传，而完成时已 rename 成 dest | 下载成功后持久化 `DownloadDone+Size+Checksum`；恢复时 dest 尺寸/校验相符则跳过下载 |
| C6 | **P1** | 归档转存**无写前配额探针**：写后仅 `scope.Adjust`（永不失败）⇒ 配额满仍写入 | `pkg/server/cloud_download_handler.go:1020`（仅 ReserveSpace+锁）、`:1052-1061`；对照 `pkg/cloud/transfer.go:857-877` 有 `ProbeReserve` | 写前 `ProbeReserve(totalSize)`（键 `user/<rel>`），失败 507 且不落对象 |
| C7 | **P1** | 写锁 30min 被回收且 release 无 token：长事务（远端大文件转存/跨卷 move）互斥失效；**旧持有者的 defer 会删掉新持有者的锁** | `pkg/server/uploading_lock.go:86,88-91,101-111`；`handlers_endpoints.go:100`；`server_timeouts.write` 默认 0 | value 存 holder token；reclaim 与 release 都用 `CompareAndDelete`（release 只删自己的）；或持锁期间续租 |
| C10 | **P1** | 加密卷**O(offset) 解密**：分块下载与 `Seek` 都「重开+CopyN」 | `pkg/files/chunked_download.go:275`；`pkg/storage/root.go:465-485`（`seekStart`）⇒ `/download` Range 同样中招 | 按块头顺序跳整块（只读头不解密）再解目标块；用例计数解密字节 |
| NEW-1 | **P1** | **软删失败直接 500 且不回写**（比 C8 更差） | `pkg/files/write_ops.go:2078-2085` | 失败即恢复原状态（复用 C8 的 `restoreQuarantine` 收敛点） |
| C3 | P2 | 释放侧**丢唤醒**：`Commit(actual<amount)` 与部分 `releaseUp` 直调点无广播，与 `quota.go:503-505` 自述约束矛盾 ⇒ 等待者睡满 30s 才 507 | `pkg/quota/quota.go:365,384`；`quota_writer.go:107,118,122`；`task_account.go:130` | 释放出口统一 `withWaitLock`；加门禁「withWaitLock 内禁裸 NotifyAll/释放漏通知」 |
| C4 | P2 | 卷容量池**上传路径不排队**（换卷后仍满即 507），而 mkdir/版本恢复会等 | `pkg/server/volumes.go:617-634` vs `dir_charge.go:73`、`version_store.go:192` | 换卷候选耗尽后做一次有界等待，或显式文档化为「立即失败」并在错误体给出可重试提示 |
| C5 | P2 | 反饿死机制失效：全部唤醒走 `NotifyAll`，`AgeBoost` 永不生效 | `quota.go:505-518`、`capacity/wait.go:118-124`、`staging_tracker.go:265` | 改精确 `Notify(res)`；或把该设计降级为文档事实并删除 AgeBoost 死代码 |
| C11 | P2 | 冷段尾读**跨 256KiB 边界整条丢失**（每边界 1 条；现有用例都落在单块内 ⇒ 假绿） | `pkg/audit/cold.go:322,346-349` | 读后把起点推进到首个 `\n` 之后（不变量：切分只产出完整行）+ 构造跨边界用例 |
| C12 | P2 | `?volume=` **未校验**即进审计 ⇒ 可伪造归属，且压制单卷兜底 | `pkg/server/audit_middleware.go:141-149`；`audit.go:128`；`pkg/audit/service.go:597` | 仅 `volSet.ByName` 命中才绑定 |
| C13 | P2 | **隧道内层（localMux）**无 `auditMiddleware`/`auditVolumeMiddleware` ⇒ 经 CLI/隧道的多卷操作 Volume 恒空、无传输层兜底 | `pkg/server/routes.go:668` 只包 srvMux；`routes_setup.go:695-737` | 两个 mux 共用同一中间件组装函数；加门禁钉住「同源」 |
| C14 | P2 | `SyncBatch` 名不副实：无 ticker，仅满 256 行才 fsync，且 fsync 在全局写锁内 | `pkg/audit/journal.go:209,291,520-530,356-358` | 实现 ticker（或改名为 SyncOnBatch 并在设计文档降级），fsync 移出锁 |
| C8 | P2 | 删除回滚 5 处静默失败（文件残留 `.deleting.<nano>`，原路径丢失，无日志/指标/清扫） | `pkg/files/write_ops.go:1955/1963/1969/1977/1986,1756` | 抽 `restoreQuarantine`：失败 Warn+metric+审计；维护任务扫 `.deleting.*` 还原 |
| C9 | P2 | 回收站 meta 随迁失败静默（后果限于「恢复后失去校验」） | `pkg/files/trash.go:116`；`pkg/volume/trusted/trusted_fs.go:94/230/658` | 失败 Warn+metric（与主文件同款） |
| NEW-2 | P2 | 恢复**静默丢条目**：坏 JSON/读失败仅 Warn 跳过，不计 skipped、不 failed、不可 resume | `pkg/cloud/manager_persist.go:165,198-208` | 汇总 recovered/skipped 并上报 metric |
| NEW-3 | P2 | 等待失败与立即失败**不可区分**：`ErrTimeout`/`ErrQueueFull` 一律折叠为 `ErrStorageFull` ⇒ 丢 `Retry-After` 语义 | `pkg/quota/wait.go:87-90` | 保留原因，507 带 `Retry-After`/错误码 |
| NEW-4 | P2 | 冷段读取失败 `break` 后**返回部分结果且无 error**；`PurgeCold` 用 `context.Background()` 无超时 | `pkg/audit/cold.go:323-326`；`pkg/server/volume_retention.go:209` | 返回 `(entries, err)` 或计指标；保留期用带超时 ctx |
| NEW-5 | P2 | 加密卷每次 `OpenDecrypted` 都跑 `encPlaintextSize` 全块头扫描（每 chunk/Range 一次 O(#blocks) syscall） | `pkg/storage/root.go:115-135,381-402` | 头部大小缓存（按 inode/mtime 失效） |

## 2. 被证伪/降级（不按原报告的严重度处理）

| 原主张 | 裁决 | 真实范围 |
| --- | --- | --- |
| C11 冷段尾读「批量丢条目」P1 | 机制真、**严重度 P2** | 每跨块边界丢 **1 条**；`limit` 小时该条本在结果集外；`QueryCold/StreamColdSegment` 用 Scanner 不受影响 |
| C8「文件永久消失」P1 | **PARTIAL** | 文件仍在同目录 `.deleting.<nano>`（可人工改名恢复）；已回 500/400 + 审计 error 行 ⇒ 是「需人工干预」而非静默丢数据 |
| C9「恢复后不可读」P1 | **PARTIAL** | meta 未移动（仍在恢复目标键）；缺 meta 走「不校验直通」，非不可读 |
| C13「隧道面无审计」P1 | 机制真、**P2** | 业务审计（handler 内 Emit）仍在；缺的是**传输层兜底**与**卷维度** |
| C3「释放全面丢唤醒」 | **PARTIAL** | 主要命中 `Commit(actual<amount)` 与少数直调点，不是全部释放路径 |
| C4「卷池完全不排队」 | **PARTIAL** | 仅上传路径（换卷候选耗尽后）；mkdir/版本恢复等已走 `WaitReserve` |
| C14「SyncBatch 完全不 fsync」 | **PARTIAL** | 满 256 行会 fsync；缺的是**时间维度**（≤255 条在 page cache） |

## 3. 需修清单（建议 3 个 PR，每项含变异测试）

**PR-A 恢复与续传（最高优先，直接对应用户四条保证）**
C1 queued 落盘 + 恢复重提 · C2 `DownloadDone` 持久化 + dest 短路 · NEW-2 recovered/skipped 可观测 · NEW-3 错误语义/`Retry-After`。

**PR-B 配额与互斥**
C6 归档转存写前探针 · C3 释放统一走 `withWaitLock` · C4 卷池有界等待（或文档化 + 可重试错误） · C7 holder token + `CompareAndDelete` / 续租 · C5 精确唤醒或删死代码。

**PR-C 数据完整性与性能**
NEW-1 软删失败回写 · C8/C9 quarantine 与 trash meta 的 Warn+metric+清扫 · C10 块级 seek（+`root.seekStart`） · C11 行对齐 · C12 卷名校验 · C13 两 mux 中间件同源（+门禁） · C14 SyncBatch ticker 或文档降级 · NEW-4 冷段错误传播与超时 · NEW-5 块头扫描缓存。

**新增门禁**：① 两 mux 审计中间件同源；② `withWaitLock` 内禁裸释放/NotifyAll；③ 恢复路径必须覆盖所有「非终态」状态（表驱动，新增状态即红）。

## 4. 修复记录（全部已落地，每个都带用例与变异验证）

| 项 | 修复 | 用例 / 变异 |
| --- | --- | --- |
| **C1** 排队任务重启不续 | `CloudTask.Submitted` 在**启动 goroutine 之前**落盘；恢复时 `restart = downloading \|\| (pending && Submitted)`，只有「从未提交」的 pending 仍是孤儿 | `TestRecovery_QueuedSubmittedTaskRestarts` / 去掉 `Submitted` 条件 ⇒ 红 |
| **C2** 转存窗口整文件重下 | 下载成功即持久化 `DownloadDone/Size/Checksum`；恢复时产物尺寸相符 ⇒ 跳过下载直接转存；显式 `ResumeTask` 与「文件异常重下」清标记 | `TestReuseDownloadedArtifact`（含尺寸不符不复用）/ 去掉尺寸校验 ⇒ 红 |
| **NEW-2** 恢复静默丢任务 | `recoverTasksFromDir` 返回 skipped 并在唯一解析处累计 `metrics.RecoverySkipped`，汇总日志带 skipped | `TestRecovery_UnreadableTaskCounted` |
| **C6** 归档转存无写前配额探针 | `probeArchiveQuota`（`Scope.ProbeReserve`，探测式不排队）在 `transfer.Execute` 前执行，不足 ⇒ 507 且不落对象 | `TestCloudArchive_ToVolume_QuotaFullRejectsBeforeWrite` / 去探针 ⇒ 红 |
| **C3** 释放侧丢唤醒 | 新增 `commitReservation`（`actual<amount` 才走根等待锁）与 `releaseReserve`；替换 `quota_writer`/`task_account` 直调 | `TestCommitBelowReserveWakesWaiter` 等 3 例 / commit 走快路 ⇒ 红 |
| **NEW-3** 等待失败不可区分 | `WaitReserve` 超时保留 `ErrWaitTimeout`（同时仍 `ErrStorageFull`），HTTP 层可据此给 `Retry-After` | `TestWaitTimeoutErrorKindPreserved` |
| **C4** 卷池不排队 | 在途预留占满（`Reserved()>0`，提交/放弃后必然释放）时 `WaitReserve` 有界等待；**已提交占满仍立即 507**（无释放可能，不空等） | `TestReserveVolumePool_WaitsForInFlightReservation` / `..._CommittedFullFailsFast` / 去等待 ⇒ 红 |
| **C7** 写锁 30min 被偷 + release 无 token | 锁值改为 ` lock:<marker>:<token>`；释放与回收一律 `CompareAndDelete`；`transferWriteLock` 用 `acquireFileLockKeepAlive`（TTL/3 续期） | `pkg/server/file_lock_token_test.go` 5 例 / release 退回 `Delete` ⇒ 红 |
| **C8 / NEW-1 / C9** 删除与回收站静默失败 | 五处回滚收敛 `restoreQuarantine`（失败 Error + `quarantine_rel`）；软删失败**先回滚再报错**；回收站 meta 随迁失败 Warn | `pkg/files` 全包 |
| **C10** O(offset) 解密 | `encRootReader.SeekPlain` 只读 4B 块头 + Seek 跳过整块，仅解密目标块；`seekStart` 优先走它 | `TestEncSeekPlain_*` + 接线用例 / 退回 CopyN ⇒ 红 |
| **NEW-5** 每次 OpenDecrypted 全块头扫描 | 按「密文尺寸 + mtime」缓存明文长度（上限 4096，超出清空）；探测用独立句柄 | `pkg/storage` 回归（`TestEncryptedPhysicalSize_MatchesDiskTruth` 抓到过句柄复用回归） |
| **C11** 冷段跨块丢条目 | `carry` 拼接跨块行（较新块首残行 + 较旧块末行） | `TestReadTailCold_CrossChunkLine`（可判定构造：边界落在指定大对象行内）/ 禁用 carry ⇒ 红 |
| **C12** `?volume=` 伪造归属 | 中间件只绑定 `volSet.ByName` 命中的卷 | `TestAuditVolume_UnknownVolumeNotBound` |
| **C13** 隧道内层无审计/卷维度 | 两条链路挂同一组中间件；新增门禁 **R42** 钉住同源 | `TestR42_AuditMiddlewareOnBothMuxes` / 删隧道侧 ⇒ 红 |
| **C14** SyncBatch 无时间维度 + fsync 在锁内 | 启动 `batchFlushLoop`（默认 200ms，Close 退出）；满批次只置 `pendingFlush`，`Append` 在**锁外** fsync | `pkg/audit/batch_sync_test.go` 2 例 |
| **NEW-4** 冷段读失败静默截断 / PurgeCold 无超时 | 读失败上抛（部分结果仍返回）；保留期清理 30s 超时 | `pkg/audit` + `pkg/server` 回归 |
| **C5** NotifyAll / AgeBoost | **部分证伪**：`AgeBoost` 确实参与 `sortWaitersLocked` 的**有效优先级**，不是死代码；`NotifyAll` 只唤醒 `canSatisfy` 的等待者（惊群有界）。已把口径写进 `pkg/waitq` 注释，不改语义 | — |

新增门禁：**R42**（两 mux 审计中间件同源）。其余两条建议门禁（`withWaitLock` 内禁裸释放、恢复路径覆盖全部非终态状态）尚未实现，登记为后续。

验证：workspace `go test ./...` **96 包** · `-tags=e2e ./test/...` 3 包 · `make web-test` · `golangci-lint` 0 ·
archcheck 全绿（含 sleep/serial 棘轮：新增固定等待已改为 `Gosched`/通道，串行用例按规范登记）。

## 5. 恢复功能的**端到端**测试覆盖（真实进程 kill -9 / 重启）

新增 `test/e2e_restart_harness_test.go`（可重启的真实 sproxy 子进程：构建一次 + 固定端口 + 同一存储根）
与两组用例：

| 用例 | 验证的保证 | 关键断言 |
| --- | --- | --- |
| `TestE2E_Restart_CloudDownloadResumesAfterKill9` | 下载中断后**自动续传** | kill -9 重启后任务 completed；源站收到**非零起点 Range**；产物 checksum 与源一致 |
| `TestE2E_Restart_QueuedTasksContinue` | **排队任务**重启后继续（C1） | `cloud_max_concurrent: 1` 下 3 任务，kill 后重启 ⇒ 三个全部 completed（修复前排队任务被转 failed） |
| `TestE2E_QuotaFull_WaitsThenSucceeds` | 配额不足**自动等待重试** | 配额满时上传不立刻 507；删除旧文件后排队上传自动成功（客户端零重试）且内容正确 |
| `TestE2E_Restart_QuotaMatchesDiskTruth` | 崩溃后**配额账本 = 磁盘真值** | 1.2 MiB/2 MiB 配额下重启：再写 1.2 MiB 必须被拒（账本没归零）；删除后可写（没幽灵占用） |
| `TestE2E_GracefulShutdown_ResumesDownload` | **优雅停机**（SIGINT）后仍能继续 | 停机重启后任务 completed（不因优雅退出丢任务） |

> 🔎 **这组 E2E 立刻抓到一处真缺陷**：`SubmitAndStart` **没有**把 `CloudTask.Submitted` 落盘
> （早前一次变异还原把该行清掉了，而单元测试是**手写**字段 ⇒ 覆盖不到写入侧）。E2E 首跑即报
> 「排队任务重启后被判 failed」，修复后新增单测 `TestSubmitAndStart_PersistsSubmitted` 钉住写入侧。
> 结论：**恢复语义必须用真实进程 E2E 验证**，单测无法替代（它验证的是「机制」而不是「接线」）。

