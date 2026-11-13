<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# IaC provider（11.10-⑦）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-iac-provider.md` 已于 2026-11-13 归档。
> 用法以 `docs/deploy.md`（IaC 总览）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
chart 原先只有基础资源（Deployment/Service/PVC/ConfigMap/values），无 Ingress/Secret/HPA/PDB，
也无 Terraform/Ansible 层。目标：补齐三层 IaC，默认全关零行为变化。

## 定案与取舍（仍有效）
1. **新增资源一律 `if enabled` 默认 false**：既有 `helm install` 零 diff（golden 渲染对比锁死）
   ——「默认关」是避免已有部署被意外改写的关键。
2. **Secret 不进 ConfigMap 明文**：以 `stringData` 注入环境变量（`SPROXY_API_KEYS` /
   `SPROXY_METRICS_TOKEN` / `SPROXY_TUNNEL_KEY`）；Terraform 值用 `sensitive = true` 标注
   （tfstate 不明文展示）。
3. **三层互补、边界清晰**：Helm=应用描述 / Terraform=基础设施声明（包装 helm_release）/
   Ansible=裸机/VM（systemd|docker + config 模板 + restart handler）。文档给出选择指南。
4. **CI 只做静态检查**：helm lint/template 渲染断言（默认产物不含 Ingress/Secret）、terraform
   validate/fmt、ansible `--syntax-check`——不部署真集群（无网络/凭据需求）。

## 落地指针
`deploy/sproxy-helm/templates/{ingress,secret,hpa,pdb}.yaml` + `NOTES.txt` + `values.yaml`；
`deploy/terraform/{modules,examples}`；`deploy/ansible/{playbook.yml,roles/sproxy}`。
