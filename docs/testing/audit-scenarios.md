# 审计场景覆盖矩阵（设计 §9.1）

> 口径（设计 §9.1）：每个业务动作的审计场景用例应**逐项断言** `action` / `actor` / `object` /
> `volume` / `result`。本文件是**活文档**：新增埋点或新增用例后请同步更新。
> 设计正文：`docs/designs/2026-10-19-audit-service.md` §9.1 / §9.2。

## 1. 判定方法

「已覆盖」= 存在测试在**同一用例内**产生该动作并断言其审计行（不是"测试文件里出现过这个词"）。
判定命令（可重复执行）：

> ⚠️ **常量陷阱**：部分动作名以**常量**定义（如 `auditActionCredAKAdd = "credential_ak_add"`），
> 只检索字面量会把已有测试误判为 GAP ⇒ 必须同时按常量名检索（`grep -rn 'auditActionCred' --include=*_test.go`）。

```bash
# 埋点动作清单（非测试源码）
grep -rhoE 'Action: *"[^"]+"' --include=*.go pkg/ | sed 's/.*"\(.*\)"/\1/' | sort -u
grep -rhoE 'recordFileAudit\([^,]+, *"[a-z_]+"' --include=*.go pkg/files/ | sed 's/.*"\(.*\)"/\1/' | sort -u
```

## 2. 覆盖矩阵（2026-10-20 快照：**59/59 埋点动作已覆盖**）

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
| `cloud_group_create` / `cloud_group_delete` | `pkg/server/audit_cloud_group_scenarios_test.go`（**并修掉「删任务误记 cloud_group_delete」+ 真组删除无痕**） | ✅ |

### 2.3 卷管理（**最大缺口**）

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `volume_move` | `pkg/server/audit_volume_scenarios_test.go`（本文件引入） | ✅ |
| `volume_rebalance` | `pkg/server/audit_volume_admin_scenarios_test.go` | ✅ |
| `volume_copy` | 同上（跨卷复制断言 object 含文件名） | ✅ |
| `volume_import` / `volume_export` | `pkg/server/audit_volume_export_scenarios_test.go` | ✅ |
| `volume_mirror` | `pkg/server/audit_mirror_scenarios_test.go`（`mirror_targets` + 直驱 `volumeMirrorPass`） | ✅ |
| `volume_tier_downgrade` / `volume_tier_promote` | `pkg/server/audit_tier_scenarios_test.go`（直驱 `scanOnce` + 直接驱动读路径决策点 `resolveTierPromote`） | ✅ |
| `user_volume_create` / `user_volume_delete` | `pkg/server/audit_user_volume_scenarios_test.go`（**并修掉外部卷建卷无审计**） | ✅ |

### 2.4 密钥 / presign / 分享 / 同步 / 凭据

| 动作 | 断言用例 | 状态 |
| --- | --- | --- |
| `secret_create` / `secret_delete` / `secret_export` | `pkg/server/audit_secret_scenarios_test.go`（含**明文红线**与失败留痕） | ✅ |
| `backend_presign` / `backend_presign_complete`（含被拒） | `pkg/server/audit_presign_scenarios_test.go`（成功 + 404 被拒） | ✅ |
| `share_create` / `share_revoke` | `pkg/server/audit_share_scenarios_test.go`（object=token） | ✅ |
| `sync_task_create` | `pkg/server/audit_sync_scenarios_test.go` | ✅ |
| `sync_conflict_resolve` | `pkg/server/audit_sync_conflict_scenarios_test.go`（注入真实冲突记录后走真实 handler） | ✅ |
| `mesh_read` / `mesh_write` | `pkg/remote/remote_read_test.go`、`remote_write_test.go` | ✅ |
| `credential_register` / `_register_denied` / `_ak_delete` / `_sk_delete` / `_persist_error` | `pkg/server/register_handler_test.go`、`credentials_handler_test.go`（**按常量断言**） | ✅ |
| `credential_ak_list` / `_ak_add` / `_sk_list` / `_renew` / `_sk_expire` | `pkg/server/audit_credential_scenarios_test.go`（并断言**不含 SK 明文**） | ✅ |
| `credential_login` / `credential_login_denied` | `pkg/server/audit_login_scenarios_test.go`（TOTP 成功 + 未知 nonce 被拒；不含 nonce/密钥本体） | ✅ |
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
| ~~P1~~ ✅ | ~~`secret_*`（3）~~ | 已补：`audit_secret_scenarios_test.go`（明文红线 + 失败留痕） | — |
| ~~P1~~ ✅ | ~~`backend_presign*`（2，含被拒）~~ | 已补：`audit_presign_scenarios_test.go`（状态码观察器覆盖全部失败分支） | — |
| ~~P2~~ ✅ | ~~卷管理 rebalance/copy/import/export/user_volume~~ | 已补（`audit_volume_admin_scenarios_test.go`、`audit_volume_export_scenarios_test.go`、`audit_user_volume_scenarios_test.go`） | — |
| ~~P2~~ ✅ | ~~`share_*`（2）~~ | 已补（`audit_share_scenarios_test.go`） | — |
| ~~P2~~ ✅ | ~~`sync_task_create`~~ | 已补（`audit_sync_scenarios_test.go`） | — |
| ~~P2~~ ✅ | ~~`sync_conflict_resolve`~~ | 已补（注入真实冲突记录 + 真实 handler） | — |
| ~~P2~~ ✅ | ~~`cloud_group_create` / `cloud_group_delete`~~ | 已补（`audit_cloud_group_scenarios_test.go`） | — |
| — | **无剩余 GAP** | 59/59 埋点动作均有断言 | — |
| ~~P2~~ ✅ | ~~`credential_login`~~ | 已补（`audit_login_scenarios_test.go`） | — |
| P3 | `ai.embed` | **未埋点**（`ActionAIEmbed` 常量存在但全仓无 `Action:` 使用点） | 先决定是否要该事件；要则补埋点 + 断言（**不含文本内容**） |

## 4. 编写纪律（§9.3）

- 默认 `t.Parallel()`；**禁新增 `time.Sleep`**（R14 棘轮），用注入 Clock / `Stats()`；
- 断言对象与形态一致（不是"文件里出现过动作名"）；关键契约配**变异验证**；
- 新增串行用例（真 kill / 端口绑定）必须登记 `internal/archcheck/serial_budgets.tsv` 并同步
  `docs/testing/virtual-time-conversions.md`（R18）；
- 复用既有 harness，不新建一套 HTTP/卷装配：`newAuditTestServer`（`audit_export_test.go`）、
  `newVolumesAPIServer`（`volumes_api_test.go`）、`assertAuditAction`（`pkg/files/audit_scenarios_test.go`）。
