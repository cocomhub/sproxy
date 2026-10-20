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
