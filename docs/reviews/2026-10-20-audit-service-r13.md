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

## 2. 待办（按严重度登记，未在本轮修复）

| 级别 | 问题 | 证据 | 建议 |
| --- | --- | --- | --- |
| P1 | `/api/audit` 读面**全量解析**热段 + 冷段（`limit` 未下推：`journal.Read` 不截断、`QueryCold(..., 0)` 永不早停），且热段读取持 `j.mu` 写锁 ⇒ 一次管理员查询可阻塞全部审计写入 | `audit_handler.go:82-95`、`journal.go:528`、`cold.go:245` | 把 `limit` 下推（`EntryFilter.Limit` + 冷段剩余预算）；热段改「尾部倒序读」或走环形索引；`Read` 接 `ctx` |
| P1 | `audit.Metrics` 生产未注入（恒 `NullMetrics`）⇒ `audit_dropped`/`write_failed`/`hook_panics` 全部不可观测（「有界丢弃」无通道） | `routes_setup.go:100-113`、`journal.go:419` | 加 `audit.Metrics → server.Metrics` 适配器并注入；丢弃路径补限流 Warn |
| P1 | 归档目的地不可用（卷名写错/未就绪）只有一条 Warn，运行期归档关闭，而封段仍被 `max_archives` 删除；读面静默只返回热段（无「不完整」信号） | `routes_setup.go:860-885`、`journal.go` `shiftArchives` | 装配期区分「未配置」与「配置了但解析失败」后者 fail-fast（或一次性告警 + 计数）；删除封段前要求已确认归档 |
| P2 | 归档 `os.Remove(seg.Path)` 与并发轮转存在 TOCTOU（归档期间发生 1~2 次轮转 ⇒ 可能删掉刚搬上来、尚未归档的新段） | `archive.go`（`archiveOne`）、`journal.go:shiftArchives` | 归档前把段 rename 到独占名再处理；或让归档与轮转共用封段互斥 |
| P2 | `Entry.RequestID`/`SourceIP` 生产未接线；`Entry.Volume` 非测试源码零赋值（卷只以 `detail="volume=..."` 文本出现） | `routes_setup.go:CtxFields`、`pkg/files/write_ops.go` | 装配层注入提取器（trace/RemoteAddr）；`RecordAudit` 增 `Volume` 字段 |
| P2 | `mesh_read`/`mesh_write` 把**目标 owner** 塞进 `Actor`（`AuditEvent` 无 Owner 字段）⇒ 代表他人操作时归属记错 | `remote_read.go:110`、`remote_write.go:103`、`audit.go:92` | `AuditEvent` 增 `Owner`（缺省回填 `ActorFrom(ctx)`） |
| P2 | 审计 TTL 只清热段；冷段（归档）**永不按龄删除**（`archive_keep=0` 时热段里根本没有超龄数据 ⇒ TTL 实际未生效） | `volume_retention.go:195-207`、`journal.go:Rewrite` | 冷段按 `ColdSegment.Last` 过期删除，或文档明确「TTL 仅作用热段」并禁止与归档同用 |
| P2 | `ai.embed` 埋点存在但**无生产调用方**（`pkg/aiembed` 尚无消费方）⇒ 该动作实际不会产生；R39 只看「符号被引用」，包装函数内引用即可转绿 ⇒ **假绿** | `ai_embed_wiring.go`、`internal/archcheck/audit_action_gate_test.go` | R39 升级为「引用点在启动可达链上」（或 deadcode 式可达性）；否则登记并删动作 |
| P2 | 设计声明的 R35-b（变更型路由必须经 `auditMiddleware`）/R35-e（读面同门同过滤）**无门禁实现**，但代码注释仍宣称 | `audit_middleware.go:21-27`、设计 §8.2 | 实现或改注释为「待实现」 |
| P2 | `MarkDone` 先置内存 `done` 再落盘；best-effort 写失败返回 nil 且**永不重试** ⇒ 协调器误报已提交，下轮重启重复执行 | `service.go:375-400` | `KindDone` 落盘失败时回滚 `done` 并向上返回错误；或区分「内存 done / 已提交」 |
| P2 | `OnSeal` 归档在**写路径同步**执行且用 `context.Background()`（无超时/不可取消）⇒ 某次业务 `Emit` 可能被整段上传拖住 | `routes_setup.go:818`、`journal.go:fireSealIfPending` | 回调改「标记 + 后台单飞 worker」或加超时 ctx |
| P3 | 死代码：`rewriteAuditLog`/`filterAuditLines`/`writeAuditLinesAtomic`（约 70 行零调用）；注释仍指向不存在的 `legacy.go` | `volume_retention.go:209-283`、`audit/core.go:16` | 删除死代码 + 订正注释 |
| P3 | `audit.OpenRange` 非测试零调用（`QueryCold` 只走流式）⇒ 「按需区间读」能力未被使用 | `pkg/audit/cold.go:172` | 在读面接上（与 P1 limit 下推同一改动）或删除 |
| P3 | 归档卷写入未接容量/配额账本（`audit-archive/` 增长不计入卷配额） | `routes_setup.go:875` | 视产品口径决定是否 `WrapStagingQuota` |

## 3. 复核结论

- 修复后：`pkg/audit` / `pkg/recovery` / `pkg/server` 定向用例全绿；
  完整验证见修复提交说明（workspace 模式 `go test ./...` + `-tags=e2e ./test/...` + `golangci-lint`）。
- 未发现会让功能立即不可用的 P0 残留；上表 P1/P2 均为「默认配置或特定运维配置下才暴露」的
  风险，按优先级后续单独提交处理。
