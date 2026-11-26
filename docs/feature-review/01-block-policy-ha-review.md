# 审查：批次 1——secretdata 分块规则引擎与整文件单块（PR #1317）

- **批次**：1
- **审查基线**：master `580361d1b`（PR #1317 squash 合并）
- **审查方法**：四维度（正确性 / 可用性 / 安全性 / 可维护性）逐路径源码审查 + 聚焦测试 + 生产高可用面专项
- **维度覆盖**：正确性 ✅ / 可用性 ✅ / 安全性 ✅ / 可维护性 ✅

## 结论

**总评**：有条件通过（功能正确、门禁全绿；生产高可用面有 2 项 P1、3 项 P2 需补强）
**发现数**：P0 0 / P1 2 / P2 3 / P3 2

## 发现清单

### [P1-1] 卷级 `block_policy.mode: single` 会让 >64 MiB 文件**整卷写失败**（配置错误晚到写时暴露）

- **位置**：`cmd/sproxy/secret_register.go:235`（白名单放行 single）、`pkg/cryptox/shardseal/block.go`（`SinglePlanner.Plan` 拒 >64MiB）
- **问题**：卷级 `single` 不携带"超过上限怎么办"的语义 ⇒ 任何 >64 MiB 文件上传都在写路径末尾 fail-closed；`max_upload_bytes` 默认 1 GiB，所以运维按直觉写下 `mode: single` 后，大文件会**持续失败**且只有运行期日志可见。规则形态（`block_rules` + `max_size`）天然表达"超限落到 random"，卷级形态无法表达。
- **建议**：装配期对卷级 `mode: single` **fail-closed**，文案指向 `extra.block_rules`（含 `max_size`）。配置错误在启动暴露，而不是写文件时才失败。

### [P1-2] 多实例下规则**不会自动传播**，且没有运维收敛入口

- **位置**：`pkg/volume/secretdata/backend_settings.go`（`SetSettingsStore` 只在装配期载入一次）、`pkg/server/volumes_api.go:1086`（`refreshExternalVolume` 只重建索引）
- **问题**：两个实例共享同一 StateStore 时，A 保存规则后 B 仍用旧规则写 ⇒ 同卷不同实例产出**不同块布局**（读不出错，但确定性/秒传命中率下降）。目前唯一收敛手段是重启 B。
- **建议**：`POST /api/volumes/refresh?volume=<卷>`（既有入口）在后端实现 `SettingsStoreSetter` 时**顺带重载规则**——不新增 API，复用既有运维通道。

### [P2-1] `Settings()` 回显只有运行期规则，看不出"生效来源"与"实际上限"

- **位置**：`pkg/volume/secretdata/backend_settings.go:38`（`Settings`）
- **问题**：清空运行期规则后回显 `rules: []`，但实际生效的是静态基线或卷默认——运维无法区分"没配"与"配了但回落到基线"。`max_size_limit` 字段只声明了类型，值为空，UI 无法提示单条规则的真实上限（`min(64MiB, 卷块上限)`）。
- **建议**：回显增加 `baseline_rules` / `effective_source` / `limits{hard_max, volume_block_max, effective_max}`（保留既有 `rules`/`version` 字段，向后兼容）。

### [P2-2] 删除 config 声明卷后同名重建会继承旧规则

- **位置**：`pkg/server/volume_settings_api.go`（`forgetVolumeSettings` 仅接入用户卷删除）
- **问题**：用户卷删除已清理；config 卷（运维改 `volumes[]` 移除再重建同名）无清理路径 ⇒ 继承旧规则。属既有残留状态风险，但影响面小（需要同名 config 卷重建）。
- **建议**：文档化 + 在 `AttachVolumeSettingsStores` 日志中提示"该卷从持久化状态继承了规则"。不改删除路径（config 卷生命周期由运维掌控，静默删配置更危险）。

### [P2-3] 本地兜底 `StateStore` 的 CAS 只保证**进程内**互斥

- **位置**：`pkg/state/local.go:309`（`LocalStateStore.CAS` 用 `sync.Mutex`）
- **问题**：默认单机用本地 StateStore 兜底卷设置；若同 `storage_root` 起**多个实例**，CAS 跨进程无效（后写覆盖先写）。这是既有 `LocalStateStore` 的既定语义（与分享/checksum 台账同源），非本功能引入。
- **建议**：文档明确"多实例必须配 `state_store: mongo`（或集群模式）"；本地兜底 = 单进程语义。不新造跨进程锁（会与 `pkg/server/filelock` 形成第二份实现）。

### [P3-1] `OnBlockModeResolved` 只到 `slog.Debug`，生产默认级别下不可观测

- **建议**：接指标计数（命中/未命中 × 模式），便于回答"规则是否真的生效"。

### [P3-2] `erasure` + `single` 的装配期告警只覆盖 `block_rules`，不覆盖运行期规则

- **建议**：热更新写入含 single 规则时同样告警一次。

## 通过项（无问题面）

- **权限门**：他人私有卷 404 防枚举、系统/共享卷需 admin、拒绝路径记审计（测试锁定 `TestVolumeSettings_PermissionMatrix` / `_PrivateUserVolumeOfOthersIs404`）。
- **输入校验**：正则 RE2（无 ReDoS）、条数/长度/size 上限、`single` 必须有 `max_size`、错误带规则下标。
- **内存上限**：`SinglePlanner` 硬顶 64 MiB 覆盖**所有**构造入口（含不经 `policy.Compile` 的配置路径，测试锁定）。
- **并发**：规则快照 `atomic.Pointer`，热更新与写路径无锁竞争（`-race` 全量通过）。
- **持久化**：CAS 版本取自持久化值（多节点追加写不互相 409）、落盘先于热生效、失败保留基线不阻断挂载。
- **JSON 响应/审计**：全走 `internal/httpjson`；审计 `result` 用契约值、detail 脱敏（不落正则原文）。
- **前端**：全部 `textContent`/`createElement`（无 innerHTML 注入）；409 冲突自动重读；Esc/遮罩/✕ 均可关闭（e2e 锁定）。
- **门禁**：R38 路由分类、R35-b 变更路由审计、R55 API 文档、R35-a 审计调用点（本轮顺带修掉 worktree 假绿）全绿。

## 验证方式

- 源码逐路径审查：`backend_settings.go` / `block_policy.go` / `policy.go` / `settings_state.go` / `volume_settings_api.go` / `volume-drawer.js`
- 聚焦测试：`go test -run 'TestVolumeSettings|TestBackend_|TestCompiled_|TestSinglePlanner' ./pkg/volume/secretdata/ ./pkg/server/ ./pkg/cryptox/shardseal/...`
- CI（PR #1317）：17 项全绿

---

## 修复记录（本批次已实施）

| 编号 | 结论 | 验证 |
|---|---|---|
| P1-1 | **已修**：卷级 `block_policy.mode: single` 装配期 fail-closed，错误文案指向 `block_rules` | `TestSecretdataOptions_VolumeLevelSingleRejected` RED→GREEN；变异（改判定为 `"MUTATED"`）→ 红 |
| P1-2 | **已修**：`POST /api/volumes/refresh?volume=X` 顺带重载卷设置（多实例收敛入口，不新增 API） | `TestRefreshVolume_ReloadsBlockPolicy` RED→GREEN |
| P2-1 | **已修**：回显新增 `baseline_rules`/`effective_source`/`limits`（并修正基线不再冒充「运行期快照」） | `TestBackend_Settings_ReportsEffectiveSourceAndLimits` + `TestBlockPolicyView_EffectiveSourceForEmptyRuntime` RED→GREEN；变异（来源判定恒 false）→ 红 |
| P2-2 | **文档化**：config 卷同名重建继承规则属运维可控边界（删配置更危险） | `docs/secret-volume.md` §3.4.1 |
| P2-3 | **文档化**：本地兜底 StateStore = 单进程语义；多实例须配 mongo | `docs/secret-volume.md` §3.4.1、`pkg/volume/secretdata/README.md` |
| P3-1 | 未实施（指标化留待可观测性批次） | — |
| P3-2 | 未实施（热更新侧的 erasure×single 告警） | — |
