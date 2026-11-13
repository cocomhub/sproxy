<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 多副本不中断（Helm）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-multireplica-helm.md` 已于 2026-11-13 归档。
> chart 行为与用法以 `docs/deploy.md`（§1 Helm）为准（现行事实源），本文件只保留**设计意图 /
> 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

`helm upgrade` 时 `deployment.yaml` 无 `strategy` 段 ⇒ 默认 `Recreate` ⇒ 升级期间服务中断；
节点排水（`kubectl drain`）也可能一次性带走全部副本。目标：滚动发布与自愿中断都**不中断**。

## 定案与取舍（仍有效）

1. **`maxUnavailable: 0` + `maxSurge: 1`**：旧 Pod 服务到新 Pod Ready 才终止；单副本下与 `Recreate`
   行为等价（零回归）。
2. **readiness 用 `/readyz`、liveness 用 `/healthz` 分工**：`/readyz` 才反映依赖（UploadStore 等）
   就绪——否则「容器起来但依赖没好」会被计入可用副本，滚动升级照样 503。`/healthz` 只表进程存活，
   用在 liveness 上避免依赖抖动导致重启风暴。
3. **PDB `minAvailable: 1`**：自愿中断（排水/驱逐）时保底一个 Ready 副本。
4. **fail-closed 的边界校验**：`replicaCount=0` 与 `minAvailable=1` 冲突 ⇒ 模板 `required` 拒绝渲染，
   不静默产出无效 manifest。
5. **不新建 Service**：ClusterIP + Deployment selector 天然支持多副本。
6. **测试形态**：用 `helm template` 渲染结果做断言（结构文本断言 + 变异验证），不引入 kind/minikube
   作为 CI 依赖。

## 落地指针

`deploy/sproxy-helm/templates/deployment.yaml`（`strategy` + 探针）、`templates/pdb.yaml`
（`policy/v1` PodDisruptionBudget + `required` 校验）、`values.yaml`（`strategy` / `pdb` 段）、
断言测试 `deploy/sproxy-helm/tests/ingress_test.go`。

## 实测差异（以代码为准）

- 设计写「`values.yaml` 新增 `strategy` 字段允许覆盖」——已落地（`strategy.rollingUpdate.*`）。
- 设计的「集成（可选）：kind/minikube 上观察 Pod 滚动」**未做**（仓库无该集成测试）。
- 本设计与[多副本写面限制](./2026-09-24-multireplica-write-limits.md)配套：「不中断」只承诺
  滚动/排水窗口，不解决多副本共享 PVC 的写冲突。
