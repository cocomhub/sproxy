# 门禁（结构门禁 > 运行时用例）

> 设计依据：`docs/designs/2026-10-19-audit-service.md` §8.2、`docs/designs/2026-10-19-waitq.md` §8.3、
> `docs/designs/2026-10-19-audit-wal-program.md`（R37）。本文件是**索引**：编号 → 断言所在的测试位置；
> 每个门禁的**准确语义**以该测试的文档注释为准（本文件不重复描述，避免两处漂移）。

## 1. 运行方式

```bash
# 全部结构门禁（CI 必检路径之一）
go test ./internal/archcheck/

# 单个门禁（例：R37 统一恢复）
go test ./internal/archcheck/ -run TestRecoveryGate_R37 -v
```

门禁目录扫描口径统一为**根 module**：遍历 `pkg/` `cmd/` `internal/`，下探遇到自带 `go.mod` 的
嵌套 module 即停（`pkg/volume/ext/*` 是独立 module，不在根 module 构建面内）。

## 2. 基线（棘轮）文件

门禁的「当前允许量」登记在 TSV 中；**只减不增**。刷新基线：

```bash
ARCHCHECK_WRITE_BASELINE=1 go test ./internal/archcheck/ -run '<门禁测试名>'
```

刷新前必须确认：新增项是**真实存在**的（而不是把断言放宽）；基线变化要在提交信息里说明原因
（含「为什么不是做假工作」）。历史教训：基线**漏登记**会产生假绿（2026-10-20 刷 R14 时发现 4 个
真实串行 e2e 从未登记）。

| 文件 | 锁定内容 |
| --- | --- |
| `audit_calls.tsv` | R35-a：审计调用点棘轮（唯一提供方 `pkg/audit`，非测试源码调用面白名单） |
| `audit_action_allowlist.tsv` | R39：审计动作常量必须有**可达**的非测试使用点（防「死常量」与「只被无调用方的包装引用」两种假绿；清单**防腐化**，已有可达引用即须删登记） |
| `external_fs_calls.tsv` | 外部文件系统调用点登记（R26 卷根直连适配器棘轮） |
| `openread_notexist.tsv` | OpenRead 的 not-exist 契约调用点登记 |
| `quota_acquire_sites.tsv` | R36-a：配额/容量获取入口棘轮（adapter / probe / pending，只减不增） |
| `quota_capability_wiring.tsv` | 配额能力装配登记（装饰器能力透传） |
| `quota_waitlock.tsv` | R34：配额释放路径必须持 waitMu（丢唤醒唯一结构性闭合） |
| `recovery_handlers.tsv` | R37：统一恢复清单（唯一编排入口 + legacy 棘轮 + 防腐化） |
| `route_classes.tsv` | R38：路由分类表（tunnel / auth / user / reader / public，双向一一对应） |
| `serial_budgets.tsv` | R14/R18：串行测试预算棘轮（新增测试默认 `t.Parallel()`） |
| `set_root_calls.tsv` | `SetRoot` 调用点棘轮（存储根装配唯一入口） |
| `usage_provider_calls.tsv` | 用量出口调用点登记（唯一用量提供方） |
| `waitq_resources.tsv` | R36-c：`waitq.Resource` 适配器登记（资源名/单位/超时/上界/优先级） |

## 3. 编写门禁的纪律（来自多轮评审）

1. **断言对象要与锁定的形态一致**：只锁「方法调用」就不要把定义行/接口声明算进计数
   （R36-a 曾因此让 `pending` 虚高近一倍）。
2. **不得假绿**：每新增/收紧一条门禁，必须做**变异验证** —— 故意违反一次，确认门禁**变红**，
   然后还原（恢复用 `cp` 备份「修复后」版本，不用 `git checkout`，以免连带毁掉无关改动）。
3. **报错信息要区分原因**：「未登记」与「登记在非法位置」是两回事，混在一起的报错会误导修复方向。
4. **清单不得腐化**：登记行必须引用仍然存在的文件/符号；代码搬迁后遗漏更新 ⇒ 门禁要能报出来。
5. **复用同包既有 helper**（`moduleRoot` / `waitqGoFiles` / `readTSVRows`），不要再写第三份目录遍历。
6. **任何接受 `*testing.T` 的 helper 首行必须 `t.Helper()`**（`thelper` linter 强制）。
7. TSV 分隔符统一用 `|`（`\t` 在 heredoc/多层转义里会被写成字面量，导致解析成 1 列）。

## 4. 门禁索引（按**文件声明**：文件头注释中声明的编号）

> 说明：本表由 `internal/archcheck/*_test.go` 的**文件头注释**机械提取（每个文件只取它自己声明的
> 编号，因此不会把跨文件互引的行当成归属）。语义仍以该文件的文档注释为准。

| 门禁测试文件 | 声明编号 | 头部说明（首条注释） |
| --- | --- | --- |
| `audit_api_gate_test.go` | R35 | audit_api_gate_test.go 是门禁 R35-d：`pkg/audit` 只能有**一套**审计 API。 |
| `audit_action_gate_test.go` | R39 | R39：**审计动作常量必须有可达的非测试使用点**（两层判据：符号被引用 + 引用它的函数本身可达）。 |
| `audit_calls_gate_test.go` | R30 · R31 · R34 · R35 | audit_calls_gate_test.go 是门禁 R35-a：审计调用点**棘轮**（只减不增）。 |
| `audit_types_gate_test.go` | R35 | audit_types_gate_test.go 是门禁 R35-c：**审计类型/常量/接口的唯一实现处 = pkg/audit**。 |
| `capability_parity_gate_test.go` | R21 | capability_parity_gate_test.go 是「透明装饰器必须声明全部可选能力」的门禁（R21）。 |
| `dead_symbols_test.go` | R11 | dead_symbols_test.go 是「**已确认删除的死代码不得复活**」的墓碑门禁（R11）。 |
| `docs_cli_flags_test.go` | R15 | docs_cli_flags_test.go 是「sclient 全局选项文档不漂移」的门禁（R15）。 |
| `docs_rules_test.go` | R9 | docs_rules_test.go 是「**协作与实施规则文档不得腐烂**」的门禁（R9）。 |
| `duplication_test.go` | R1 · R5 | duplication_test.go 是**重复实现门禁**：把「共享辅助必须走单一事实源」从文档约定变成 |
| `external_fs_gate_test.go` | R27 | external_fs_gate_test.go 是「外部卷后端 FS 直连点」门禁（R27）。 |
| `flock_gate_test.go` | R19 · R23 | flock_gate_test.go 是「直接 flock 调用不得用于代码」的门禁（R23）。 |
| `gate_wiring_test.go` | R13 | gate_wiring_test.go 是「**门禁自身可用性**」的守卫（R13）。 |
| `http_transport_gate_test.go` | R19 · R20 | http_transport_gate_test.go 是「裸 http.Transport 构造不得用于代码」的门禁（R19） |
| `inner_escape_gate_test.go` | R22 | inner_escape_gate_test.go 是「`Inner()` 逃生口仅限能力探测/装配层」的门禁（R22，第 7 轮 |
| `ledger_mutation_gate_test.go` | R32 | ledger_mutation_gate_test.go 是「**账本变更必须全在唯一临界区**」门禁（R32）。 |
| `notest_gate_test.go` | R17 | notest_gate_test.go 是「**`make notest` 门禁自身可用性**」的守卫（R17）。 |
| `quota_capability_wiring_gate_test.go` | R31 | quota_capability_wiring_gate_test.go 是「**决策驱动的配额能力必须有生产接线**」门禁（R31）。 |
| `quota_waitlock_gate_test.go` | R31 · R32 · R33 · R34 | quota_waitlock_gate_test.go 是**门禁 R34**：释放/放宽可用额度的公开入口必须在根等待锁 |
| `recover_ordering_gate_test.go` | R33 | recover_ordering_gate_test.go 是「**恢复必须先完成 map 写入、之后才启动 goroutine**」门禁（R33）。 |
| `recovery_gate_test.go` | R37 | recovery_gate_test.go 是门禁 **R37**（设计 docs/designs/2026-10-19-audit-service.md §8.2 + |
| `release_policy_test.go` | R12 | release_policy_test.go 是「**CHANGELOG 单一事实源不得被回退成手工维护**」的门禁（R12）。 |
| `repo_hygiene_test.go` | R16 | repo_hygiene_test.go 是「开源仓库卫生文档」门禁（R16）。 |
| `repo_walk_test.go` | R11 · R14 · R18 | repo_walk_test.go 是「全仓源码遍历类门禁」**共用的目录排除口径**（单一事实源）。 |
| `route_classification_gate_test.go` | R38 | route_classification_gate_test.go 是门禁 **R38**：**路由分类表 + deny-by-default**。 |
| `set_root_accessor_gate_test.go` | R26 · R27 · R28 | set_root_accessor_gate_test.go 是「`Set.Root(...)` 卷根访问器」棘轮门禁（R28）。 |
| `state_store_gate_test.go` | R19 · R21 | state_store_gate_test.go 是「状态落盘必须走 StateStore 抽象」的门禁（R21）。 |
| `storage_volume_wrap_gate_test.go` | R26 · R27 · R28 · R29 | storage_volume_wrap_gate_test.go 是「整卷容量包装唯一入口」门禁（R29）。 |
| `submodule_test.go` | R1 · R2 · R4 | submodule_test.go 把 R2（子包可见性）与 R4（领域包不得导入装配层）扩展到**子 module**， |
| `sync_cond_gate_test.go` | R36 | sync_cond_gate_test.go 是门禁 **R36-b**：非测试源码**不得**再出现 `sync.Cond` |
| `test_parallel_gate_test.go` | R18 | 门禁 R18「测试并发注册」——用户明示 2026-09-16： |
| `test_sleep_ratchet_test.go` | R14 | test_sleep_ratchet_test.go 是「测试内固定等待只减不增」的**棘轮门禁**（R14）。 |
| `upload_side_effect_test.go` | R8 | upload_side_effect_test.go 是「**上传成功的副作用必须单一实现**」的门禁（R8，P2-d 配套）。 |
| `usage_provider_gate_test.go` | R30 | usage_provider_gate_test.go 是「卷用量查询**单一查询面**」门禁（R30，2026-10-17）。 |
| `user_io_gate_test.go` | R25 | user_io_gate_test.go 是「用户内容读写必须经用户可见入口」的门禁（R25，第 5 轮对抗评审 |
| `volume_root_bypass_gate_test.go` | R26 | volume_root_bypass_gate_test.go 是「**卷根直连适配器**」门禁（R26；仓库的 R 号按设计文档 |
| `waitq_gates_test.go` | R36 | waitq_gates_test.go 汇总 W1 收敛的两条门禁（设计 docs/designs/2026-10-19-waitq.md §8.3）： |
| `web_assets_test.go` | R10 | web_assets_test.go 是「**Web 前端每个 JS 文件都必须被自动化测试覆盖**」的门禁（R10）。 |
