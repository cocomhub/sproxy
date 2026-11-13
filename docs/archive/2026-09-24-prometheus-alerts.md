<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Prometheus 告警规则模板（roadmap 11.5-⑪）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-prometheus-alerts.md` 已于 2026-11-13 归档。
> 规则清单与接入步骤见 `docs/grafana/alerts/README.md`（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
官方 dashboard 有「卷 I/O 失败率」面板但**没有阈值告警**，也没有磁盘水位与备份同步指标。
目标：以**部署级** Prometheus rule 模板交付告警（磁盘水位 / 卷 degraded / 备份同步失败 / 云下载失败率），
并补齐缺口指标。

## 定案与取舍（仍有效）
1. **告警模板是部署期静态文件，不做运行时热加载**：sproxy 侧不为此维护一套规则下发/校验通道；
   代价是改阈值要改文件 + reload Prometheus，可接受（阈值调整是低频运维动作）。
2. **阈值保守、warning 先行**（磁盘 0.85 warning / 0.95 critical、卷 IO 失败率 5m > 0.05、
   备份同步失败 15m > 0、云下载失败率 10m > 0.2）：宁可晚报也不制造噪声，否则告警会被忽略。
3. **模板变量与 dashboard 同款 `{{ $job }}`**：运维只需在一个地方改 job 名，避免两套命名口径。
4. **指标缺失 ⇒ expr 空 ⇒ 天然不告警**（Prometheus 行为）：不必为「旧版本没有该指标」写兜底逻辑，
   只需在 README 标注最低版本并在升级后自动生效。这是刻意的「不写代码解决问题」。
5. **规则语法必须 fail-closed 拦截在 CI**：YAML 可解析 + expr 内 metric 名对注册表**白名单**校验
   （写错指标名会得到永远不触发的哑规则，这种错误在运行期完全不可见）。

## 与内置告警引擎的关系（容易混淆，务必保留）
sproxy 内置 `alerts`（进程内阈值引擎 + 通知中心渠道）与本模板是**两个不同层次**的能力：
内置是**进程内**视角（单实例自省），模板是**部署级**视角（多副本/故障域聚合）。二者可同时启用、互不冲突。
新增告警时应先判断属于哪一层，不要重复实现。

## 依赖与外部契约
- 依赖指标命名（`sproxy_storage_usage_bytes` / `sproxy_storage_capacity_bytes` /
  `sproxy_backup_tasks_failed_total` / `sproxy_backup_tasks_total` 等）—— 改名即破坏模板。
- 外部系统：Prometheus ≥ 2.0（`rule_files`）、Alertmanager ≥ 0.15（v2 API）、可选 Grafana alerting provisioning。
- 规则名（`Sproxy*`）与 `severity` 标签是接收侧路由依据（外部契约）；改规则需同步 README 阈值说明。

## 落地指针
`docs/grafana/alerts/alert.rules.yml`、`docs/grafana/alerts/README.md`、
`docs/grafana/alerts/alert_rules_test.go`（YAML + metric 白名单 + 模板渲染断言）。
