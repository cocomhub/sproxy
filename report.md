# REPORT.md —— 运维闭环线（L1）

## 状态

**DONE**（PR #584 已创建，CI 进行中）

## 分支 / 提交

- 分支：`feat/ops-closure`（worktree `.worktrees/feat/ops-closure`，基于 origin/master）
- 提交 1（功能）：`ae997ce6f` — `feat(ops): 运维闭环——审计轮转 + Prometheus 告警规则 + Helm Ingress/多副本 + 优雅重启`
- 提交 2（roadmap 状态）：`f9b1989a4` — `docs(roadmap): 运维闭环 L1 四项（11.5-⑥⑪⑫/11.6-②③）状态更新为已落地`
- PR：https://github.com/cocomhub/sproxy/pull/584

## 改动文件（24 个）

### 审计日志轮转（11.5-⑥）
- `pkg/server/config.go` / `config_defaults.go` / `config_validate.go`：`audit.max_size`（ByteSize，默认 0=关闭轮转零回归）+ `audit.max_archives`（默认 3）；负值 Validate 拒绝
- `pkg/server/audit_store.go`：`AuditStore` 新增 maxSize/maxArchives；Append 持锁内超限 `rotateLocked`（关句柄→归档移位 audit.log→.1→…→.N→重开新文件，超 maxArchives 删最旧）；失败记日志继续 append
- 恢复语义固化：NewAuditStore 只载入当前 audit.log（热历史有界），归档冷数据不载入内存
- `pkg/server/routes.go`：装配传入 RotationConfig

### Prometheus 告警规则模板（11.5-⑪）
- `pkg/server/metrics.go`：新 gauge `sproxy_storage_usage_bytes/capacity_bytes{volume}`（卷集合注入；容量 0=无限不输出）+ 计数 `sproxy_backup_tasks_total/_failed_total`
- `docs/grafana/alerts/alert.rules.yml`：4 类规则（磁盘 0.85/0.95、卷 IO 失败率 >5%、备份同步失败、云下载失败率 >20%），`job=~"$job"` 模板
- `docs/grafana/alerts/README.md`：接入步骤 + Grafana provisioning 示例 + 版本要求

### Helm Ingress/TLS + 多副本（11.5-⑫ / 11.6-③）
- `deploy/sproxy-helm/templates/ingress.yaml`：v1，enabled 门控 + ingressClassName + hosts 循环（pathType Prefix）+ tls 块 + cert-manager 注解 + required fail-closed
- `deploy/sproxy-helm/templates/pdb.yaml`：policy/v1，minAvailable + selector 与 deployment 对齐；replicaCount=0 冲突 required
- `deploy/sproxy-helm/templates/deployment.yaml`：strategy RollingUpdate{maxUnavailable:0, maxSurge:1} + readinessProbe 改 /readyz（livenessProbe 保留 /healthz）
- `deploy/sproxy-helm/values.yaml`：strategy/pdb/ingress 段（默认关/1 副本零回归）

### sproxy 优雅重启（11.6-②，Unix-only）
- `cmd/sproxy/graceful_restart_unix.go`：USR2 → startRestartChild（ExtraFiles fd 3 + SPROXY_INHERIT_FD）→ waitRestartReady（轮询 127.0.0.1:<port>/readyz，超时 max(60s, shutdown)）→ 复用 handleSignalShutdown drain；spawn/超时 fail-safe
- `cmd/sproxy/restart_signal_{stub,unix,windows}.go`：平台收口（Windows 无 SIGUSR2 特性关闭）
- `cmd/sproxy/root.go`：restartListener 存取 + USR2 分支

### 门禁 / 文档
- `internal/archcheck/test_sleep_ratchet_test.go`：sleep 棘轮 +1（fd helper 子进程 accept 轮询，已登记）
- `docs/testing/virtual-time-conversions.md`：串行用例登记（优雅重启包级 restartListener）
- `docs/roadmap.md`：11.5-⑥⑪⑫ / 11.6-②③ 状态 → 已落地

## 测试证据

| 文件 | 覆盖 |
|---|---|
| `pkg/server/audit_rotation_test.go` | 边界 ==/>、归档内容、maxArchives 修剪、max_size=0 零回归、重启只载当前、并发 1000 事件不丢行 |
| `pkg/server/volume_watermark_metrics_test.go` | 水位 gauge 输出 + 备份计数 + nil 安全 |
| `docs/grafana/alerts/alert_rules_test.go` | YAML 解析、规则数 ≥4、metric 白名单、job 模板、0.85/0.95 阈值、severity/annotations |
| `deploy/sproxy-helm/tests/ingress_test.go` | 9 条模板断言（门控/TLS/注解/required/strategy/探针/PDB/零副本） |
| `cmd/sproxy/graceful_restart_unix_test.go` | USR2 判定、fd 继承（helper FileListener 同地址）、readyz 就绪/超时、编排就绪后 drain/超时不 drain、无 listener 不自杀 |

## 变异验证（删关键逻辑 → 测试红 → 还原）

- 审计轮转：`>`/`>=` 互换 → 红；漏 Rename → 红；不修剪最旧档 → 红；无条件轮转（max_size=0 也轮）→ 红；load 读归档 → 红
- 优雅重启：删就绪等待直接 drain → 红；超时仍 drain → 红；子进程回退 net.Listen（不继承 fd）→ EADDRINUSE → 红

## 本地验证（全绿）

```
go build ./...                                     OK
go test -count=1 -race（相关包 5 个）               全绿（46s/4.9s/1.5s/1.5s/19.8s）
go test ./internal/archcheck/                      全绿（sleep 棘轮 + 串行棘轮 + 覆盖探针）
golangci-lint run                                  0 issues
goimports -l                                      干净
make vet / make notest / make deadcode-check       全绿
GOOS=linux/darwin/windows go build ./cmd/sproxy/   交叉编译通过（优雅重启 Unix-only，Windows 桩编译）
```

## CI 状态

- PR #584：https://github.com/cocomhub/sproxy/pull/584
- Conventional Commits format：pass；Detect docs-only：pass
- 完整 CI（Build×6 / Test×2 / E2E×2 / Lint / Test Sub-Modules / UI E2E）进行中
- 等 CI 全绿后由主 agent squash 合并
