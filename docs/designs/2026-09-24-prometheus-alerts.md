# Prometheus 告警规则模板（11.5-⑪）设计

## 背景 / 目标
- 现状（sproxy-dashboard.json 实证）：16 面板（requests/5xx/volume I/O/cloud/hub/mux），有「卷 I/O 失败率」面板但无阈值告警；无磁盘水位与备份同步指标。
- 目标：提供告警规则模板 `docs/grafana/alerts/alert.rules.yml`（三类：磁盘水位 / 卷 degraded / 备份同步失败），与 dashboard 同款 `{{ $job }}` 模板变量；补齐缺口指标。

## 组件与接口
- 新文件：
  - `docs/grafana/alerts/alert.rules.yml`（Prometheus rules，`{{ $job }}` 模板同 dashboard）。
  - `docs/grafana/alerts/README.md`（接入：prometheus.yml rule_files + Grafana provisioning 示例 + 版本要求）。
  - `docs/grafana/alerts/alert.rules_test.go`（Go 测试：YAML 解析 + metric 白名单断言 + 模板渲染）。
- 指标缺口（pkg/server/metrics 增量，gauge，需实现时核对现有注册表）：
  - `sproxy_storage_usage_bytes{volume}` / `sproxy_storage_capacity_bytes{volume}`（卷磁盘水位）
  - `sproxy_backup_tasks_failed_total` / `sproxy_backup_tasks_total`（配合备份功能）。

## 规则清单（expr + for + severity）
1. `SproxyStorageUsageHigh`：`sproxy_storage_usage_bytes / clamp_min(sproxy_storage_capacity_bytes,1) > 0.85`，for 10m，warning；同 expr `> 0.95`，critical。
2. `SproxyVolumeHighErrorRate`：`sum(rate(sproxy_volume_io_failures_total{job=~"$job"}[5m])) by (instance) / clamp_min(sum(rate(sproxy_volume_io_total{job=~"$job"}[5m])) by (instance), 1) > 0.05`，for 5m，warning。
3. `SproxyBackupSyncFailures`：`increase(sproxy_backup_tasks_failed_total{job=~"$job"}[15m]) > 0`，for 5m，warning。
4. `SproxyCloudDownloadFailureRate`：cloud 任务失败率 > 0.2，for 10m，warning。
- 每条：labels{severity} + annotations{summary, description}（含 job/instance 模板）。

## 数据流
sproxy /metrics → Prometheus 采集 → rule_files 加载模板（渲染 $job）→ Alertmanager。规则为部署时静态模板，不做运行时热加载。

## 错误处理
- 规则语法错误：CI 测试在合并前拦截（YAML 结构 + metric 白名单 fail-closed）。
- 指标缺失（版本未升级）：expr 空 → 不告警（Prometheus 天然行为）；README 注明最低版本。

## 测试 + 变异点
1. YAML 可解析、rules ≥ 4 条（变异：删规则 → 红）。
2. 每条 expr 内 metric 名 ∈ 白名单（从 pkg/server metrics 注册表提取；变异：expr 写错名 → 红）。
3. `{{ $job }}` 模板渲染后合法（变异：漏模板变量 → 红）。
4. 磁盘规则 0.85/0.95 阈值存在（变异：改阈值 → 红）。

## 片划分
- P1：新 metrics（storage/backup 计数）+ rules 文件 + 测试。
- P2：README + Grafana provisioning 示例 + 与备份功能接线。

## 风险与零回归保证
- 纯新增文件 + 新增 gauge（Prometheus 加新名/label 无兼容问题）；dashboard 不动。
- 阈值保守（warning 先行），规则顶部注释给调整指引。
