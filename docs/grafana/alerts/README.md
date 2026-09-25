# Prometheus 告警规则模板

> 覆盖 roadmap 11.5-⑪。与 `../sproxy-dashboard.json` 同款 `{{ $job }}` 模板变量：
> 采集 job 名为 `sproxy` 时无需改；其它 job 名在 `prometheus.yml` 的
> `rule_files` 加载时用 `{{ $job := "sproxy" }}` 前缀渲染即可。

## 规则清单（docs/grafana/alerts/alert.rules.yml）

| 规则 | expr 摘要 | for | severity |
|------|-----------|-----|----------|
| `SproxyStorageUsageHigh`（warning） | `sproxy_storage_usage_bytes / clamp_min(sproxy_storage_capacity_bytes,1) > 0.85` | 10m | warning |
| `SproxyStorageUsageHigh`（critical） | 同 expr `> 0.95` | 10m | critical |
| `SproxyVolumeHighErrorRate` | `sum(rate(sproxy_volume_io_failures_total{job=~"$job"}[5m])) by (instance) / clamp_min(sum(rate(sproxy_volume_io_total{job=~"$job"}[5m])) by (instance), 1) > 0.05` | 5m | warning |
| `SproxyBackupSyncFailures` | `increase(sproxy_backup_tasks_failed_total{job=~"$job"}[15m]) > 0` | 5m | warning |
| `SproxyCloudDownloadFailureRate` | `sum(rate(sproxy_cloud_tasks_failed{job=~"$job"}[10m])) by (instance) / clamp_min(sum(rate(sproxy_cloud_tasks_created{job=~"$job"}[10m])) by (instance), 1) > 0.2` | 10m | warning |

> 指标依赖：`sproxy_storage_usage_bytes` / `sproxy_storage_capacity_bytes` /
> `sproxy_backup_tasks_failed_total` / `sproxy_backup_tasks_total` 由 sproxy
> **v0.21.0+**（运维闭环线）输出；旧版本缺指标时对应 expr 恒空 → 不告警
> （Prometheus 天然行为），升级版本后自动生效。

## 接入步骤

1. **Prometheus**：`prometheus.yml` 加 `rule_files`：

   ```yaml
   rule_files:
     - /etc/prometheus/rules/alert.rules.yml
   ```

   若采集 job 名不是 `sproxy`，在 rules 顶部加渲染前缀：

   ```yaml
   # alert.rules.yml 顶部（模板渲染前）
   {{ $job := "my-sproxy-job" }}
   ```

2. **Alertmanager**：rules 内 `alertmanager: sproxy` 标签可改为你自己的
   route/receiver 名（按 labels 路由）。

3. **Grafana（可选，直接展示告警）**：alerting provisioning：

   ```yaml
   # grafana/provisioning/alerting/sproxy.yml
   apiVersion: 1
   contactPoints:
     - orgId: 1
       name: sproxy
       receivers:
         - uid: sproxy-contact
           type: webhook
           settings:
             url: https://example.com/hook
   policies:
     - orgId: 1
       receiver: sproxy-contact
       group_by: ['alertname']
       matchers:
         - alertname =~ "Sproxy.*"
   ```

## 版本要求

- Prometheus ≥ 2.0（`rule_files` 语法）。
- Alertmanager ≥ 0.15（v2 API）。

## 阈值调整

- 磁盘水位 warning 0.85 / critical 0.95：`alert.rules.yml` 中按需修改。
- 卷 IO 失败率 0.05、云下载失败率 0.2：同文件修改后 `promtool check rules` 校验。

## 与内置告警引擎的关系

sproxy 内置 `alerts` 配置（通知中心渠道）是**进程内**告警；本模板是**部署级**
Prometheus 告警（多副本/故障域视角）。两者可同时启用互不冲突。
