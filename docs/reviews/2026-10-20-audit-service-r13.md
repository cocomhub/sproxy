# 审计服务 · R13 对抗评审（合并后复核）

- 评审对象：`e81619a9a`（PR #833 合并后的 master）中的审计 program
- 评审方式：三路 fresh-context 只读评审（正确性/生产可用性 · 收敛/完备性 · 可维护性/完整性），
  逐条**证伪**后进入修复；修复分支见文末。
- 评审基线：`git diff e81619a9a^..e81619a9a`（180 文件 / +15406 −3024）

## 1. 已修复（本文件随修复提交一起落地）

| 级别 | 问题 | 证据 | 修复 |
| --- | --- | --- | --- |
| **P0** | 冷段归档用「日期 + basename」做目标名 ⇒ 同一天内同名不同内容**撞名**；`WriteIfAbsent` 对已存在返回 `(false, nil)` 被当成功 ⇒ 调用方删除本地封段 = **审计证据静默丢失** | `pkg/audit/archive.go`（旧 `Archive`）；`pkg/sync/entry.go` 契约 | 改为**内容寻址**：临时名 → 边写边算 SHA-256 → rename 到 `<basename>.<sha256:16>`；rename 命中已存在且 Stat 成功 ⇒ 幂等成功（同内容）；否则报错不删本地。回归用例改为「异内容 ⇒ 两个对象都在」（旧用例锁的正是丢数据的判定） |
| P1 | `audit.max_total_bytes` 注释/文档承诺「默认 1 GiB 非零安全默认」，`Default()` 未赋值 ⇒ 实际 0（不限） | `pkg/server/config_defaults.go`、`config.go:185` | `Default()` 补 `MaxTotalBytes: 1<<30` + 用例 `TestAuditDefaults_MaxTotalBytesIsBounded` |
| P1 | `audit.buffer_size: 0` 直接 `return nil, nil` ⇒ **连落盘一起关掉**（整条审计链静默关闭），与文档「只关内存环形缓冲」相反 | `pkg/server/routes_setup.go` | 改为只换 `audit.NullIndex`（`indexEnabled`），journal/service 照常装配 + 用例 `TestAuditServices_BufferSizeZeroKeepsJournal` |
| P1 | 启动恢复在「只有 legacy、无作用域注册」时仍**全量扫描审计段**（守卫写成 `len(regs)==0 && len(legacies)==0`），并让 `system` 作用域永久报「未注册前缀」 | `pkg/recovery/recovery.go:listUnfinished`、`pkg/server/routes.go:238` | `listUnfinished` 增 `len(c.regs)==0 ⇒ 不扫描` + 用例 `TestRun_NoScopeRegistrationsSkipsScan`（legacy 照常执行） |
| P2 | 归档段判据用 `Contains(base, ".audit.log.")`，而生产段名是 `audit.log`（不含前置点）⇒ `Rewrite`/`Rotate` 会误处理归档段 | `pkg/audit/journal.go` | 改用 `archiveIndex(base) > 0`（并按 `audit.log.<n>` 解析，兼容内容寻址后缀）+ 用例 `TestRotateRewrite_SkipArchivedWithProductionName` |
| P2 | 自造 `Result: "failed"` 第四值（UI 只有 success/denied/error 三档 ⇒ 查不到） | `backends_api.go`、`secrets_api.go`、`volumes_api.go` | 统一改 `AuditResultError` |
| P3 | `Entry.StartMS` 被赋成 `DurMS`（步骤开始时间字段恒等于耗时） | `pkg/audit/service.go` | 改 `start.UnixMilli()` |
| P3（一致性） | 覆盖率矩阵把「传输层 `request_id`/`source_ip`」标为 ✅，但**中间件无测试文件**、且这两个字段生产**从未接线**（恒空） | `docs/testing/audit-scenarios.md`、`routes_setup.go:CtxFields` | 矩阵改为 ⬜ GAP 并写明原因 |

## 2. 第二轮修复（同分支后续提交）

| 级别 | 问题 | 修复 |
| --- | --- | --- |
| P1 | `/api/audit` 读面**全量解析**且**持 journal 写锁**（一次管理员查询阻塞全部审计写入）；冷段 `limit` 恒传 0 ⇒ 早停永不触发 | `FileJournal.Read` 改为「锁内只 flush + 快照段名，解析在锁外」；新增 `ReadTail(ctx,f,limit)`（新段优先、收满即停）；读面把**剩余预算**下推给 `QueryCold` |
| P1 | `audit.Metrics` 生产未注入（恒 `NullMetrics`）⇒ `audit_dropped`/写失败/钩子 panic/轮转不可观测 | 新增 `auditMetrics` 适配器 + `Metrics.AuditDropped/Evicted/WriteFailed/HookPanics` + `sproxy_audit_*` 指标渲染；装配层注入（`setupAuditServices(..., audit.Metrics, ...)`） |
| P1 | 归档目的地不可用仅一条 Warn，运行期归档关闭而封段仍被 `max_archives` 回收 | 装配期显式校验：`archive_volume` 解析失败 ⇒ **Error**（`startup.fail_fast=true` 时 panic）；解析函数本身不再逐请求 Warn（避免日志放大） |
| P2 | 归档 `os.Remove(seg.Path)` 与并发轮转 TOCTOU（可能删掉刚产生、未归档的新段） | 新增 `FileJournal.QuarantineSegment`（**锁内**改名为 `<name>.archiving.<n>`），归档只读/删隔离名；失败尽力放回原名，绝不删除 |
| P2 | `Entry.RequestID`/`SourceIP` 未接线（条目恒空） | 新增 `audit_ctx.go`（ctx 携带客户端 IP + trace id 提取器）；`requestLog` 注入 IP；`CtxFields` 接线；`AuditEvent` 投影带出 |
| P2 | `mesh_read`/`mesh_write` 把**目标 owner** 塞进 `Actor` ⇒ 归属记错 | `AuditEvent` 增 `Owner`（显式优先，否则回退 `ActorFrom(ctx)`）；两处改为 `Owner: tgt.owner` |
| P2 | `MarkDone` 先置内存 `done` 再落盘，best-effort 吞错且**永不重试** ⇒ 协调器误报已提交、重启重复执行 | `writeMode/persistMode(strict)`：提交记录落盘失败**回滚 `done` 并上抛错误**（协调器记 Failed，下轮重试）；用例同步改为断言「必须报错 + 可重试」 |
| P2 | `OnSeal` 归档在写路径同步执行且 `context.Background()`（无超时/不可取消） | 归档加 `auditArchiveTimeout=5min` 超时（超时本轮放弃、封段保留、下轮重试） |
| P3 | 死代码 `rewriteAuditLog`/`filterAuditLines`/`writeAuditLinesAtomic`（约 70 行零调用） | 删除 |

## 3. 第三轮修复（纯工程决策项）

| 级别 | 问题 | 修复 |
| --- | --- | --- |
| P2 | 审计 TTL 只清热段；冷段（归档）**永不按龄删除**（`archive_keep=0` 时 TTL 实际未生效） | 新增 `audit.PurgeCold(ctx, fs, root, cutoff)`（**时间未知的段保守保留**；单个失败不影响其余）；`retentionCleanAudit` 在 `Rewrite` 之后按同一 cutoff 清理冷段；用例 `TestPurgeCold_*` |
| P2 | **R39 假绿**：只被「无调用方的导出包装」引用的动作（`ai.embed` / `NewEmbedClient`）也能转绿 | R39 增加**一层可达判据**：引用该常量的函数名必须出现在「其它非测试文件」或本文件内出现 >1 次，否则判「假绿」；`ActionAIEmbed` 登记 allowlist 并写明「接入消费方后必须删除登记」；变异等价验证：该判据恰好只命中 `ai.embed`（其余 20 个动作无误报） |
| P2 | 归档不可用时封段仍被 `max_archives` 回收 | `FileJournal.SetArchiveUnavailable`：装配层在「归档已配置但 coldFS 解析失败」时置位 ⇒ `shiftArchives` **整轮跳过**（只跳删除不够：后续 rename 会覆盖最旧段）⇒ 证据保留，磁盘由 `max_total_bytes` 兜底；用例 `TestShiftArchives_RetainsWhenArchiveUnavailable` |

## 3.1 第三轮（续）：门禁与卷维度

| 级别 | 问题 | 修复 |
| --- | --- | --- |
| P2 | `Entry.Volume` 从未赋值；读面无该维度 ⇒ 取证无法回答「哪个卷」 | 卷做成 **ctx 第 6 维**（`ContextFields.Volume` + `fillContext` 唯一提取）；`auditVolumeMiddleware`（`?volume=`）+ DAV/S3 解析点绑定；语义「显式 > ctx > 单卷兜底 > 空」，多卷未绑定**留空**（不猜默认卷）；读面 `EntryFilter.Volume` + `?volume=` + 投影 + UI 列/筛选 + 真浏览器 e2e |

**变异验证（7 命中）**：R35-b 删登记 / 意造动作名 / pending 超预算 / 动作无场景证据；R35-e 去 export 门 / 裁决读 URL / 去 PrincipalFrom 优先。

## 4. 剩余待办

**全部关闭**（R13 共 21 项：第一轮 8 + 第二轮 9 + 第三轮 3 + 本轮 3 大项）。

最后两项的处理记录：

| 原项 | 结论 |
| --- | --- |
| `audit.OpenRange` 非测试零调用 | 接到生产：`GET /api/audit/cold/seg` 区间读（仅 admin、`path` 走 `ListCold` 白名单、区间夹紧 64KiB–4MiB、`Content-Range` 续读）；同时新增 `ReadTailCold`（修 newest-first 结果集错误 + 只读最新段尾部），`probeSegmentTimes` 早已复用 `RangeReader` |
| 归档卷写入未接配额账本 | **前提被推翻**：容量记账已由 `capacity.CapacityFS`（FS 层，唯一 `LedgerCounter`）覆盖，`PurgeCold` 的 `Delete` 自动释放 ⇒ 无需新账本。真缺陷是**对账抹账**（`adjustVolumePool` 按 tenant 桶校准，`audit-archive/` 不在桶内）⇒ 用**归档卷专用性校验**（非默认卷 + 白名单为空的专用卷）结构性消除，并同时满足证据隔离；R41 门禁钉住前缀不被挂到用户面 |

## 5. 复核结论

- 修复后：`pkg/audit` / `pkg/recovery` / `pkg/server` 定向用例全绿；
  完整验证见修复提交说明（workspace 模式 `go test ./...` + `-tags=e2e ./test/...` + `golangci-lint`）。
- 未发现会让功能立即不可用的 P0 残留；上表 P1/P2 均为「默认配置或特定运维配置下才暴露」的
  风险，按优先级后续单独提交处理。
