<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Helm Ingress/TLS 补全（roadmap 11.5-⑫）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-helm-ingress.md` 已于 2026-11-13 归档。
> 使用方式（`--set` 示例、生产建议）见 `docs/deploy.md`（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
chart 原本只有 Deployment + Service(ClusterIP) + PVC + ConfigMap，**无 Ingress、无 TLS 管理** ——
公网访问要用户自建 Ingress。目标：补齐 Ingress + TLS（手动 secret 或 cert-manager），
且**默认全关**。

## 定案与取舍（仍有效）
1. **默认 `enabled: false` ⇒ 渲染输出与旧 chart 逐字一致**：这是零回归的判据（已部署集群可原样复测），
   也符合「配置默认值必须安全」的纪律 —— 不新增默认开启的能力。
2. **TLS 两条路径并存**：手动 `secretName`（用户自管证书）或 cert-manager 注解（`clusterIssuer`）。
   不做「二选一强制」——两种运维方式在真实集群中都是常态。
3. **关键参数缺失时渲染期 fail**（helm `required`）：`tls.enabled=true` 而 `secretName` 为空、
   `certManager=true` 而 `clusterIssuer` 为空 ⇒ 直接报错。宁可 `helm template` 失败，
   也不要生成「TLS 块缺失/注解空值」的非法资源让问题漂到运行期。
4. **Service 保持 ClusterIP 不动**（Ingress 指向 Service），`service.type` 仍可切 NodePort/LoadBalancer ——
   补 Ingress 不应顺带重构 Service 暴露模型。
5. **与 sproxy 内置 TLS 的关系（易踩坑）**：Ingress 在边缘终结 TLS 时后端走 HTTP，
   应关掉内置 `tls.enabled`，否则出现**双层 TLS**（证书校验/重定向语义混乱、排障困难）。
   该互操作说明必须留在用户可见文档中（已并入 `docs/deploy.md`）。

## 依赖与外部契约
- 资源 API 版本 `networking.k8s.io/v1`；外部依赖 = 集群 Ingress Controller 与（可选）cert-manager。
- values 键名与 `helm template --set` 的形状是外部契约（渲染快照断言锁定）：
  `ingress.enabled/className/annotations/hosts/paths/tls.{enabled,secretName,certManager,clusterIssuer}`。

## 落地指针
`deploy/sproxy-helm/templates/ingress.yaml`、`deploy/sproxy-helm/values.yaml`（`ingress` 段）。
