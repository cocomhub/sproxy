<!--

SPDX-License-Identifier: Apache-2.0
-->

# 门禁棘轮削减分析与路线图（2026-11-11）

> 归档：从三份逐门禁分析报告提炼。**结论与判断保持有效**；过程性数字（各批次提交/PR 号）已省略。
> 权威索引见 `docs/gates.md`（门禁编号 ↔ 测试位置）；本文件只记录「哪些棘轮项可以真实下降、怎么降、风险」。
> 基线提交：`e72ee3003`（master）。

## 0. 门禁体系全貌

- `internal/archcheck/`：**77 个门禁测试文件** + **24 个基线 TSV**。
- 门禁分两类：**硬禁止/结构断言**（不可"降"，只能遵守，如 R54 JSON 硬禁、R23 flock、R19 transport、R36-b sync.Cond、R11 死符号墓碑）与**棘轮（只减不增）**。
- 棘轮的可降空间差异极大：多数门禁的登记项是**设计保留**（真需要该 API 或机制完整性所需），可降的是**真实重构消除**与**计数噪音**两类。

## 1. 逐门禁可降性总表

| 门禁 | 载体 | 当前 | 可降 | 方式 | 前置/风险 |
| --- | --- | --- | --- | --- | --- |
| R27 外部卷 FS 直连 | `external_fs_calls.tsv` | 28 行 / 预算 60 | 预算 60→**41**、行 28→25（本轮 51）；理论 →23 | 删陈旧登记 + 收紧预算；注释去字面量或改 AST 计数 | 注释改写低风险；改 AST 属门禁口径变更（需变异验证） |
| R28 `Set.Root` 直连 | `set_root_calls.tsv` | 12 行 / 预算 16 | 16→**14**（本轮）；→8–10 | 注释去字面量 + 收敛 `Abs`/存在性判定 helper | 改 helper 需过 `helper_impl_drift_test.go` 等价断言 |
| R25 用户内容 IO | 文件内 allowlist | 11 条 | 已删 2 条陈旧（→9）；加防腐化自检（本轮） | 删除零引用白名单 + 自检 | 低；建议后续改 AST（避免注释误报） |
| R26 `NewLocalFS` 直连 | 文件内 allowlist | 6 条 | 0（AST + 双向自检，最干净） | 设计保留 | — |
| R29 容量包装唯一入口 | 文件内 allowlist | 2 条 | 0 | 设计保留（唯一入口 + 封装卷显式例外） | — |
| R42 FS 派生状态自述 | `fs_derived_state.tsv` | 25 条 | ~1 | 给 `rootFSCore` 实现 `StateGen`（低收益） | 中；建议单独立项 |
| R46 旧派生状态使用点 | `legacy_state_usage.tsv` | 12 行 / 36 点 | 36→**5**（口径修复）；真实可降仅 5 点 | V-b：排除注释 + 文件名字面量；再降需 §15.1/§15.2 落地 | 口径修复须用 `go/parser` 注释区间，勿朴素切 `//` |
| R40 list 级缓存类型 | `list_cache_types.tsv` | 15 行 | →13（随 searchIndex→TreeView 删除 2 行） | 依赖 V4/§15.2 | §15.2 需 3 个口径决策 |
| R41 list/search 数据源 | `list_source_calls.tsv` | 1 行 | 0 | 设计保留 | — |
| R51 迁移期别名 | `alias_ratchet.tsv` | 2 文件 / 246 行 | 本轮 246→**209**；终态 0/0 | 删零引用别名（本轮 34 条）；全量迁移分 5–6 片 | 全量迁移牵动 ~200 文件（config_alias 的 `Config` 别名最大） |
| R55 API 文档 | `docs_api_undocumented.tsv` | 已删（终态，未文档化 0） | 0 | 已终态 | — |
| R57 裸 HTTP 构造 | `http_client_budgets.tsv` | 1 行 | 本轮 →**0**（硬禁+零基线） | insecure Transport 上移 `pkg/netutil.InsecureTransport()` | 低；注意 R19 只在 `netutil.go` 豁免 |
| R18 测试并发 | `serial_budgets.tsv` | 290 行 / 1825 例 | 本轮修正 10 行陈旧路径；理论 →1200–1500 | 分批加 `t.Parallel()`（T1 纯单元 ~1001 例、T2 httptest ~479 例） | 每批须重生成基线锁定收益；synctest/真浏览器/真子进程/固定端口不可并行 |
| R14 测试睡眠 | `test_sleep_ratchet_test.go` map | 61 处 / 预算 62 | →~12 | 条件轮询转 `testutil.WaitFor` | 需等待条件可观测 |
| R35-a 审计调用点 | `audit_calls.tsv` | 39 行 / 181 点 | **0 且不应降** | 是"不许新增散点"守卫 | — |
| R35-b 变更型路由审计 | `mutating_route_audit.tsv` | pending 42（顶格） | pending →36 | 为「已有 action、缺场景证据」6 条补 `Test*` 断言 | 需真实走 handler 场景 |
| R36-a 配额获取入口 | `quota_acquire_sites.tsv` | 20 行 / pending=0 | ≈0 | 余下 adapter/probe 均设计合法 | — |
| R34/R44/R31/R36-c | 各自 TSV | — | ≈0 | 设计性登记面 | — |
| R43 配置默认值 | `config_defaults.tsv` | 13 条 | →~6 | 按 AGENTS 7c 反转语义（`disable_xxx`） | **破坏性**（键名变更）；须一次性批次 + 文档/示例同步 |
| R38 路由分类 | `route_classes.tsv` | 259 行 | 不适用 | 完全一致性表（无预算） | 减少行 = 减路由（产品决策） |

> **诚实口径**：R46 的 36 点里 24 点是注释/文件名字面量噪音、6 点永久保留，真实可降只有 5 点——不要因"数字不再下降"误读为收敛停滞。

## 2. 批次 1（本轮交付，低风险）

1. `serial_budgets.tsv`：修正 **10 行陈旧路径**（`pkg/server/accept_retry_test.go` → `pkg/server/remotefs/...`；`pkg/server/config_*` → `pkg/server/config/config_*`）。**只改名不改数**（总数仍 1825），恢复逐文件棘轮有效性。
2. `external_fs_calls.tsv`：删 3 条陈旧登记（2 条已无 `.FS()`、1 条文件已删除）+ 收紧 4 处预算 → 预算 60→51、行 28→25。
3. `set_root_calls.tsv`：改写 2 处注释字面量 + 删 1 条纯注释行 + 收紧 1 处 → 预算 16→14；修正头部「剩余 12 处」过期描述。
4. `user_io_gate_test.go`：删 2 条陈旧白名单（`pkg/server/share.go`、`pkg/server/s3/server.go`，真实零引用）+ 新增**白名单防腐化自检**（登记项必须仍命中，否则红）。
5. R57 清零：新增 `pkg/netutil.InsecureTransport()`（`IsolatedTransport` 基座 + `InsecureSkipVerify`，仅本地联调），`cmd/sproxy/pikpak_register.go` 改用它；删除 `http_client_budgets.tsv` 唯一登记行 → 零基线。
6. R51：删 `pkg/server/config_alias.go` 31 条 + `pkg/server/ai_alias.go` 3 条**零引用**别名 → 246→209 行（`alias_ratchet.tsv` 同步）。
7. `docs/gates.md`：修正 R27/R28 两行索引描述（原写成 R26/`SetRoot`）。

## 3. 批次 2+（待决策/较大工程）

| 批次 | 内容 | 决策点 | 预估收益 |
| --- | --- | --- | --- |
| B2-a | R18 串行并行化（T1 纯单元先做 6–8 文件） | 无（逐包 `-race -count=2` 复核；每批重生成基线） | 串行 1825→1200–1500 |
| B2-b | R14 睡眠转 `WaitFor` | 无 | 61→~12 |
| B2-c | R35-b 补 6 条测试场景证据 | 无 | pending 42→36 |
| B2-d | R27 注释去字面量（8 个纯注释文件）**或**改 AST 计数 | **D1**：AST（更正确、零 churn） vs 注释改写（不改门禁语义） | R27 51→41/23 |
| B2-e | R51 别名全量迁移（`config_alias.go` 分 5 片 + `ai_alias.go` 2 片） | **D2**：是否排期（大片、机械但牵动 ~200 文件） | 209→0（文件消失） |
| B2-f | R46 口径修复（排除注释 + 文件名字面量） | **D3**：是否接受收窄口径（用 `go/parser` 注释区间；探针下限同步下调） | 36→5 |
| B2-g | R46/V4：台账只读化 + searchIndex→TreeView 投影 | **D4**：§15.1.3 已判"暂缓"，读路径脱离台账需先定"卷自报 checksum" vs "volumestate 承载 checksum 视图"；§15.2 需 3 个口径决策 | 归零，门禁可删 |
| B2-h | R43 配置默认值语义反转（`StateStore.Type` 先行；`TLS.Enabled`/`Pikpak.Hybrid.AutoDelete` 为 7c 目标样本） | **D5**：破坏性键名变更是否排期（须统一批次 + 示例/文档/Validate 同步） | 13→~6 |
| B2-i | R28 行为收敛（`Abs` 收口 helper、存在性判定走 `FS.Stat`） | 无（需过 helper 等价断言） | 14→8–10 |

## 4. 决策点（需用户裁定）

- **D1**：R27 计数改 AST 还是改写注释？（AST 更符合「断言对象=锁定形态」，且免 8 文件注释 churn；代价是改门禁判定法，须补变异用例）
- **D2**：R51 别名全量迁移是否排期？（终态删文件；机械但量大，`Config` 别名一处牵动 ~202 文件）
- **D3**：R46 口径收窄是否接受？（真实可降点只有 5，其余是噪音；不收窄则数字无法反映收敛）
- **D4**：R46/V4 台账只读化路线（用户口径）：读路径改「卷自报 checksum」还是「`volumestate` 承载 checksum 视图」？§15.2 TreeView 投影的 3 个口径。
- **D5**：R43 破坏性语义反转（`tls.enabled`→`disable_tls` 等）是否排期？

## 5. 仍有效的判断与踩坑

- **"陈旧登记"是棘轮族系统性风险**：R25（无自检）、R27（仅 `t.Logf`）、R46/R51 之外的文本/白名单门禁都出现过"文件已删/已迁移但登记残留"，会让棘轮**静默放宽**。修法：登记项必须有双向自检（R26/R42 是范本）。
- **门禁棘轮主循环遍历 budget**（R18）：budget 里存在、源码里不存在的路径行被静默忽略，只经"总数上限"间接生效 ⇒ 路径迁移必须改名而非删行。
- **文本计数类门禁（R27/R28/R22/R25）把注释/字符串一并计入**：导致预算虚高（R27 实测 51 中 28 处是注释）。是否改 AST 见 D1。
- **测试环境坑**：`gofmt -w` 会重排 map 对齐，删行请用精确匹配或在 gofmt 后再改；Windows 终端 `PYTHONIOENCODING=utf-8` 避免 GBK 编码错误。
- **R57 与 R58 联动**：把 insecure Transport 放到 `pkg/netutil`（R19 文件级豁免 + R57 目录豁免）是唯一"零违规"落点；若改用 `httpc.Options{Insecure:true}` 而调用点将来加 `CloseIdleConnections()` 会踩 R58。
