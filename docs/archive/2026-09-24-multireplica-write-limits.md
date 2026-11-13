<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 多副本写面限制声明 — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-multireplica-write-limits.md` 已于 2026-11-13 归档。
> 权威声明已落 `docs/deploy.md`（§1「多副本写面限制（只读面水平扩展 + 单写主）」），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

Helm chart 支持 `replicaCount` 且已补 RollingUpdate + PDB，但「多副本时写请求怎么办」**没有任何
权威声明**——共享 PVC（`ReadWriteOnce`）下多副本写冲突由谁负责、写主是哪个副本，全靠用户猜。

## 定案与取舍（仍有效）

1. **只声明、不做运行时强制**：口径是「多副本 = **只读面水平扩展**；**写面只有一个写主**」，Helm
   **不拦截**误发到读副本的写请求（防御性拦截会在 helm 层制造第二套语义，且 RWO 挂载本身就限制了）。
   违背声明的后果由部署方承担——如实披露比假装兜住更有价值。
2. **单副本零回归**：`replicaCount=1` 时唯一副本即写主，行为与单机完全一致。
3. **写主身份由编排层决定**：`Deployment` 的 Pod 序号不稳定 ⇒ 需要稳定「副本 0」时应换 `StatefulSet`
   等编排形态，再把写流量定向过去。
4. **PVC 是硬约束**：默认 `accessMode: ReadWriteOnce` 天然阻止多副本同时挂卷；要多副本必须换外部共享
   存储，而共享存储下的写冲突语义**不由本声明承担**。
5. **可观测口径**：以 `/api/mesh/status`（节点角色/地址）作为写面排障入口；与「全局只允许一个写面」
   （`cluster.role=replica` 只读副本）语义对齐。
6. **测试形态**：设计原意是用文档关键短语做断言测试（防文档被改回「多副本可写」），属**文档门禁**而非运行期。

## 落地指针

`docs/deploy.md`（§1 多副本写面限制，权威声明）、`deploy/sproxy-helm/values.yaml`（`replicaCount` /
`persistence.accessMode`）。

## 实测差异（以代码为准）

- 设计「片2」的两项**未落地**：① `values.yaml` 未新增 `writeLeader.replicaIndex`，`replicaCount`
  注释也未写「写主/只读」指引；② 未新增 archcheck 风格的文档断言测试（现存的
  `deploy/sproxy-helm/tests/ingress_test.go` 只断言 `strategy`/探针/PDB，`TestHelmChart_ValuesDoc`
  实际只查 `maxUnavailable: 0`）。
- 因此本口径目前**只靠声明与部署纪律**保证，无自动化门禁；如需收紧，应按设计补文档断言测试。
