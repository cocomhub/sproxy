# 审计场景覆盖矩阵（设计 §9.1）

> 口径（设计 §9.1）：每个业务动作的审计场景用例应**逐项断言** `action` / `actor` / `object` /
> `volume` / `result`。本文件是**活文档**：新增埋点或新增用例后请同步更新。
> 设计正文：`docs/designs/2026-10-19-audit-service.md` §9.1 / §9.2。

## 1. 判定方法

「已覆盖」= 存在测试在**同一用例内**产生该动作并断言其审计行（不是"测试文件里出现过这个词"）。
判定命令（可重复执行）：

```bash
# 埋点动作清单（非测试源码）
grep -rhoE 'Action: *"[^"]+"' --include=*.go pkg/ | sed 's/.*"\(.*\)"/\1/' | sort -u
grep -rhoE 'recordFileAudit\([^,]+, *"[a-z_]+"' --include=*.go pkg/files/ | sed 's/.*"\(.*\)"/\1/' | sort -u
```

## 2. 覆盖矩阵（2026-10-20 快照）

### 2.1 文件 / 目录（`pkg/files` + `pkg/server`）

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `upload` | `pkg/server/*`（upload 链路）、`pkg/files/audit_scenarios_test.go` | ✅ |
| `overwrite` | `pkg/files/audit_scenarios_test.go`、`pkg/syncexec/sync_test.go` | ✅ |
| `rename` | `pkg/files/audit_scenarios_test.go` | ✅ |
| `copy` | `pkg/files/audit_scenarios_test.go`、`quota_matrix_test.go` | ✅ |
| `move` | `pkg/server/volumes_api_test.go`、`pkg/files/audit_scenarios_test.go` | ✅ |
| `delete` / 软删 | `pkg/server/audit_export_test.go`、`pkg/files/audit_scenarios_test.go` | ✅ |
| `mkdir` / `rmdir` | `pkg/files/audit_scenarios_test.go`（含 rmdir 配额不足 denied） | ✅ |
| 目录配额不足 `denied` | `pkg/files/audit_scenarios_test.go` | ✅ |
| `tags` | `pkg/files/audit_scenarios_test.go` | ✅ |
| 失败不记 success | `pkg/files/audit_scenarios_test.go:TestAuditScenario_FailedOpNotRecordedAsSuccess` | ✅ |
| `purge`（AI 隐私清理） | `pkg/server/ai_privacy_handler_test.go`（`ai.privacy_purge`） | ✅ |

### 2.2 归档 / 回收站 / 版本 / 云任务

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `archive`（含 target volume） | `pkg/server/cloud_archive_quota_settle_test.go` 等 | ✅ |
| 回收站 `restore` / `empty`（单卷 + `all=1`） | `pkg/server/trash_test.go`、`trash_multivolume_test.go` | ✅ |
| `version_restore` / `version_delete` | `pkg/server/audit_test.go` | ✅ |
| `cloud_archive` / `cloud_cancel` / `cloud_delete` | `pkg/server/cloud_audit_e2e_test.go`、`audit_test.go` | ✅ |
| `cloud_group_create` / `cloud_group_delete` | — | ⬜ **GAP** |

### 2.3 卷管理（**最大缺口**）

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `volume_move` | `pkg/server/audit_volume_scenarios_test.go`（本文件引入） | ✅ |
| `volume_rebalance` | — | ⬜ GAP |
| `volume_copy` / `volume_mirror` | — | ⬜ GAP |
| `volume_import` / `volume_export` | — | ⬜ GAP |
| `volume_tier_promote` / `volume_tier_downgrade` | — | ⬜ GAP |
| `user_volume_create` / `user_volume_delete` | — | ⬜ GAP |

### 2.4 密钥 / presign / 分享 / 同步 / 凭据

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `secret_create` / `secret_delete` / `secret_export` | — | ⬜ GAP（**必须断言不含密钥明文**） |
| `backend_presign` / `backend_presign_complete`（含被拒） | — | ⬜ GAP |
| `share_create` / `share_revoke` | — | ⬜ GAP |
| `sync_task_create` / `sync_conflict_resolve` | — | ⬜ GAP |
| `mesh_read` / `mesh_write` | `pkg/remote/remote_read_test.go`、`remote_write_test.go` | ✅ |
| `credential_register` / `_login` / `_renew` / `_ak_*` / `_sk_*` | — | ⬜ GAP |
| `config_update` / `verify` / `backup` | `pkg/server/audit_test.go`、`cmd/sproxy/*_test.go` | ✅ |

### 2.5 传输层 / 读面

| 场景 | 断言用例 | 状态 |
| --- | --- | --- |
| 任意变更路由 ⇒ ≥1 条事件含 `request_id` / `source_ip` | `pkg/server/audit_middleware*_test.go`（`http.` 前缀族） | ✅ |
| 读面权限矩阵（§5.7） | `pkg/server/audit_readface_perm_test.go` | ✅ |
| 归档后仍可查询（热 + 冷合并） | `pkg/server/audit_cold_merge_test.go` | ✅ |
| Owner 越权矩阵（读/写/删/分享/回收站 ⇒ 403） | 分散于 `pkg/server/*_test.go`（按 ACL 用例） | 🟡 分散未汇总 |
| `owner` 判定用 `Owner` 而非 `Actor` | `pkg/server/audit_test.go`（owner 维度） | ✅ |

## 3. 缺口与优先级（下一步）

| 优先级 | 缺口 | 风险 | 建议用例形态 |
| --- | --- | --- | --- |
| P1 | `secret_*`（3） | **密钥泄露**：必须断言条目里**不含明文/密钥材料** | `newAuditTestServer` + 建/删/导出密钥 ⇒ 断言 action + detail 不含 secret |
| P1 | `backend_presign*`（2，含被拒） | presign 是越权高发点 | 允许/拒绝两路 ⇒ 断言 `result` 分别为 success/denied |
| P2 | 卷管理（8） | 管理员高危操作无审计断言 | 复用 `newVolumesAPIServer(t, actor, volumes, mod)` + `h.readAuditEvents` 断言（本文件已示范 `volume_move`） |
| P2 | `share_*`（2） | 外链分享 = 数据外流 | 创建/撤销 ⇒ 断言 action + object=分享 id |
| P2 | `sync_task_create` / `sync_conflict_resolve`（2） | 同步改数据 | 建任务/解决冲突 ⇒ 断言 |
| P3 | `credential_*`（6） | 认证面变更 | 复用 `register_handler_test.go` harness |
| P3 | `cloud_group_*`（2） | 组操作 | 复用 cloud group 测试 harness |

## 4. 编写纪律（§9.3）

- 默认 `t.Parallel()`；**禁新增 `time.Sleep`**（R14 棘轮），用注入 Clock / `Stats()`；
- 断言对象与形态一致（不是"文件里出现过动作名"）；关键契约配**变异验证**；
- 新增串行用例（真 kill / 端口绑定）必须登记 `internal/archcheck/serial_budgets.tsv` 并同步
  `docs/testing/virtual-time-conversions.md`（R18）；
- 复用既有 harness，不新建一套 HTTP/卷装配：`newAuditTestServer`（`audit_export_test.go`）、
  `newVolumesAPIServer`（`volumes_api_test.go`）、`assertAuditAction`（`pkg/files/audit_scenarios_test.go`）。
