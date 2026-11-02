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
| `mutating_route_audit_gate_test.go` | R35-b | R35-b：**变更型路由必须有业务语义审计**（登记表 `mutating_route_audit.tsv` 三条 join：路由↔pattern、动作名真实存在、动作有场景证据；`pending` 受预算棘轮）。 |
| `task_lifecycle_gate_test.go` | R43 | R43：**云任务状态机 × 重启恢复覆盖**（写入的状态必须登记；非终态不得 `keep`；`restart` 状态必须出现在 `restart :=` 判定式里）。 |
| `quota_direct_release_gate_test.go` | R44 | R44：**账本原语直调点登记**（`commitUp`/`releaseUp`/`releaseCommittedUp` 不加锁；新增直调点必须写明为何不丢唤醒）。 |
| `audit_mux_parity_gate_test.go` | R42 | R42：**两个 mux（主 / 隧道内层）必须挂同一组审计中间件**（`auditMiddleware` + `auditVolumeMiddleware`），防「隧道/CLI 路径无兜底审计、卷维度恒空」。 |
| `audit_archive_prefix_gate_test.go` | R41 | R41：**审计归档前缀 `audit-archive` 只能出现在归档实现/装配点**（防证据前缀被挂到用户可见面；白名单制）。 |
| `audit_readface_gate_test.go` | R35-e | R35-e：审计**读面**必须双 mux 注册 + 单一裁决点 `auditReadAllowed`（不得读请求参数放宽）+ 单一读管线 `readAuditEvents`。 |
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
| `route_classification_gate_test.go` | R38 | route_classification_gate_test.go 是门禁 **R38**：**路由分类表 + deny-by-default**。扫描面含**三类**：① `xxxMux.HandleFunc("METHOD /path", ...)`；② `xxxMux.Handle("METHOD /path", ...)` 与仅路径挂载 `xxxMux.Handle("/prefix/", ...)`（此前只扫 HandleFunc ⇒ 12 处带包装的注册对门禁隐形，含 /metrics、pprof、DAV、凭据登录面）；③ **数据化路由表**；④ **插件登录面路由**（`pkg/authn/ext/*` 的 `ExternalAuthRoute{Method, Pattern}` 字面量 + 同文件常量解析，对应装配层的 `for _, extRoute := range h.externalAuthRoutes` 动态循环——此前该循环的 pattern 是变量，对门禁完全隐形）（`pkg/server/*/routes.go` 的 `RoutePrefix` + `router.Route` 字面量表，由 `router.Register` 双 mux 注册）——子包迁入路由后仍与 `route_classes.tsv` 双向比对。| |
| `handlers_deps_gate_test.go` | R7 | R7：**Handlers 必须是分域依赖的组合**（直接字段数 ≤ 12、每个字段为嵌入的 `*Deps`；新依赖加到 deps.go 分组，禁止 god object 回归）。 |
| `server_scope_gate_test.go` | R6 | R6：**server 作用域子包不得导入装配根**（`pkg/server/**` → `pkg/server` 禁止；且每个子包必须在 Levels 登记，否则对 R1 隐形）。 |
| `set_root_accessor_gate_test.go` | R26 · R27 · R28 | set_root_accessor_gate_test.go 是「`Set.Root(...)` 卷根访问器」棘轮门禁（R28）。 |
| `state_store_gate_test.go` | R19 · R21 | state_store_gate_test.go 是「状态落盘必须走 StateStore 抽象」的门禁（R21）。 |
| `storage_volume_wrap_gate_test.go` | R26 · R27 · R28 · R29 | storage_volume_wrap_gate_test.go 是「整卷容量包装唯一入口」门禁（R29）。 |
| `submodule_test.go` | R1 · R2 · R4 | submodule_test.go 把 R2（子包可见性）与 R4（领域包不得导入装配层）扩展到**子 module**， |
| `sync_cond_gate_test.go` | R36 | sync_cond_gate_test.go 是门禁 **R36-b**：非测试源码**不得**再出现 `sync.Cond` |
| `test_parallel_gate_test.go` | R18 | 门禁 R18「测试并发注册」——用户明示 2026-09-16： |
| `test_sleep_ratchet_test.go` | R14 | test_sleep_ratchet_test.go 是「测试内固定等待只减不增」的**棘轮门禁**（R14）。 |
| `upload_side_effect_test.go` | R8 | upload_side_effect_test.go 是「**上传成功的副作用必须单一实现**」的门禁（R8，P2-d 配套）。 |
| `tempdir_gate_test.go` | R53 | 测试临时目录门禁（2026-10-31）：`*_test.go` 不得裸调 `os.MkdirTemp`/`ioutil.TempDir`，必须用 `testutil.MkdirTemp`（测试内重试 + 进程级兜底）或 `t.TempDir()`。背景：%TEMP% 累积 1,910 个本仓测试目录（Windows 上 `RemoveAll` 因句柄未释放静默失败 + 显式 TempDir 无人清理）。 |
| `json_response_gate_test.go` | R54 | JSON 响应写出**单一实现**（2026-11-01 棘轮 → 2026-11-02 **硬禁止**）：非测试源码中出现「裸 JSON 响应写」（`json.NewEncoder(w)`；同行 `w.Header().Set(` + `"application/json"`/`contentTypeJSON`）即红。背景：曾有 **5 份**等价实现（装配根 `sendJSONResponse` 328 处、`pkg/files.Service.sendJSON` 146 处、`ratelimit` 429 回包、`cloudtasks.writeJSON`、`remotefs` 两个 helper）+ 41 处裸写；现全部收敛到 `internal/httpjson`（`Send`/`SendOpt`/`SendErr`/`SendError`/`WriteBody`），基线表已删除。豁免 `internal/httpjson/`（实现本体）、`pkg/testutil/`（外部服务替身）、`cmd/`（CLI stdout，`w` 是 io.Writer）。 |
| `docs_api_gate_test.go` | R55 | API 文档与路由表一致（2026-11-02）：`docs/api.md` 的 `### METHOD /path` 标题集合必须与 `route_classes.tsv` 的路径集合对齐；未文档化路径逐条登记在 `docs_api_undocumented.tsv`（**驱动清单**，集合精确相等——补文档后必须删行，新增未文档化必须登记并写理由）；文档里出现过期路径即红。背景：文档漂移不会让任何测试变红，实测 122 条路由只覆盖 64 条。 |
| `test_count_ratchet_test.go` | R50 | 测试函数总数棘轮（2026-10-30）：全仓 `func Test*` 总数不得低于 `test_count_baseline.tsv` 基线（只增不减）。背景：#927 搬运时一个测试被静默删除（−1），无门禁发现；有意删除测试须同步下调基线并说明理由。 |
| `test_doc_comment_gate_test.go` | R49 | 测试文档注释悬空门禁（2026-10-30）：`pkg/server` 下 `// TestXxx` 文档注释若指向全仓不存在的测试函数即红。与 R50 互补（R49 抓「注释残留」，R50 抓「连注释一起删」）。 |
| `usage_provider_gate_test.go` | R30 | usage_provider_gate_test.go 是「卷用量查询**单一查询面**」门禁（R30，2026-10-17）。 |
| `user_io_gate_test.go` | R25 | user_io_gate_test.go 是「用户内容读写必须经用户可见入口」的门禁（R25，第 5 轮对抗评审 |
| `volume_backend_registration_order_gate_test.go` | R45 | 门禁 **R45**：外部卷后端**类型注册**必须早于 `assembleVolumes`（`setupServerCore` 内 `registerVolumeBackendTypes()` 先于 `server.RegisterRoutes(`），且 `setupSyncVolumeBackends` 不得再回退注册类型。背景（用户实测 2026-10-21）：baidupcs/webdav/sftp/ftp/s3 的类型此前在 RegisterRoutes **之后**注册 ⇒ config 静态声明 `type: baidupcs` 等外部卷在装配期报「未注册后端」并启动失败（只能运行期建卷绕过）。 |
| `volume_root_bypass_gate_test.go` | R26 | volume_root_bypass_gate_test.go 是「**卷根直连适配器**」门禁（R26；仓库的 R 号按设计文档 |
| `waitq_gates_test.go` | R36 | waitq_gates_test.go 汇总 W1 收敛的两条门禁（设计 docs/designs/2026-10-19-waitq.md §8.3）： |
| `web_assets_test.go` | R10 | web_assets_test.go 是「**Web 前端每个 JS 文件都必须被自动化测试覆盖**」的门禁（R10）。 |
| `alias_ratchet_test.go` | R51 | 迁移期别名文件 `pkg/server/*_alias.go` **只减不增**（文件数 + 行数双层棘轮）；清理别名后必须同步下调基线，锁定收益。 |
| `arch_test.go` | R1 · R2 · R3 · R4 | 分层与作用域的基础门禁：R1 已登记包不得导入更高层级的包；R2 子包只允许父域子树与装配层导入；R3 全仓遍历三条规则的空断言防护（实测包数 > 0）；R4 `pkg/**` 非 `pkg/server/**` 不得导入装配层。 |
| `build_flags_alignment_test.go` | — | GoReleaser 发布产物与 `make build` 的构建标志一致（仅允许 `-w -s` strip 差异），防「本地能跑、发布二进制行为不同」。 |
| `chunked_persist_serialization_test.go` | — | 分块会话的**持久化入口必须全部经会话内嵌串行锁**（并发乱序缺陷的测试是概率性的，故用源码门禁兜底）。 |
| `cloud_stop_signal_order_gate_test.go` | R48 | 云任务「停止信号必须在配额释放之后」：`waitTaskStopped` 返回 ⇒ 配额已归零（变异：把 signal 提前即红）。 |
| `cloud_wait_event_gate_test.go` | R14 配套 | 云任务等待必须**事件化**（禁「循环 + sleep 轮询」）；与 R14 测试睡眠棘轮互补。 |
| `config_defaults_gate_test.go` | R43 | 配置默认值安全：bool 默认必须 false、string 默认必须空（AGENTS.md 7c），防「默认开启」语义混淆。 |
| `delete_invalidation_gate_test.go` | R39 | 持有派生缓存的卷实现，其**删除/移动路径必须失效该缓存**。 |
| `deps_field_cap_test.go` | R52 | `pkg/server/<cap>` 切片的 `Deps` 字段数上限（默认 12）：字段数即切片的入边宽度，增长必须收敛为既有接口方法。 |
| `fs_derived_state_gate_test.go` | R42 | FS 实现必须自述派生状态（供卷状态缓存判定是否可复用），防新增 FS 静默绕过缓存语义。 |
| `index_single_write_gate_test.go` | R47 | 派生状态**单一写入口**：业务写路径不得直接写索引（只允许写内核/事件驱动），附允许清单。 |
| `legacy_state_usage_gate_test.go` | R46 | 「旧派生状态实现」使用点棘轮（只减不增），锁定向 Root 事件收敛的收益。 |
| `list_cache_gate_test.go` | R40 | list 级缓存**唯一提供方**（`pkg/volumestate`），禁止各卷各自造缓存。 |
| `list_source_gate_test.go` | R41 | list/search 数据源收口（影子台账阶段）：两段式断言，确保数据源不回流到旧的逐卷扫描。 |
| `makefile_bench_deps_test.go` | — | 任何会编译/类型检查根 module 的 Makefile 入口必须依赖 `prepare`（否则 benchmark 在缺前置时表面超时、真实根因是缺目标）。 |
| `makefile_bench_timeout_test.go` | — | benchmark 入口必须设置**包级** `-timeout`（单个 op 卡死不返回时的兜底）。 |
| `makefile_bench_watchdog_test.go` | — | `make bench` 必须经**进程外**停滞看门狗（`tools/benchwatch`），超时给出诊断而非静默挂起。 |
| `makefile_target_dup_test.go` | — | Makefile 同一目标不得定义两次（只带先决条件的重复规则会被 make 静默合并）。 |
| `openread_notexist_gate_test.go` | — | 每个 `OpenRead` 实现都必须**显式声明 not-exist 语义**（返回 `fs.ErrNotExist` 还是包装错误），防新增后端静默改变调用方分支。 |
| `release_title_pattern_test.go` | — | 发布相关标题/标签格式断言（与 release-please 配置一致性配套）。 |
| `root_mutation_gate_test.go` | R45 | `storage.Root` 结构性变异必须发事件（机制完整性在编译期之外的第二道保险）。 |
| `volume_state_gate_test.go` | R44 | 卷状态缓存**唯一装配点**（R29 同形教训：横切关注点不能变成 N 个调用点各自的义务）。 |
| `xfer_send_atomicity_test.go` | — | 传输层 `Send` 必须经 `iostream.WriteFull` 写足（防半帧），新增传输实现绕过即红。 |
