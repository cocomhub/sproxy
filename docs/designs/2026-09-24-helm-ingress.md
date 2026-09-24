# Helm Ingress/TLS 补全（11.5-⑫）设计

## 背景 / 目标
- 现状（chart 实证）：Chart.yaml v0.1.0 / appVersion 0.15.0，含 Deployment + Service(ClusterIP) + PVC + ConfigMap，无 Ingress 无 TLS 管理，公网访问需自建 Ingress。
- 目标：补 `templates/ingress.yaml` + TLS 证书管理（手动 secret / cert-manager 注解），默认全关（enabled:false）零回归。

## 组件与接口
- values.yaml 新增：
  ```yaml
  ingress:
    enabled: false
    className: ""            # 如 nginx；空 = 集群默认 IngressClass
    annotations: {}          # 用户附加注解（合并）
    hosts:
      - host: sproxy.example.com
        paths: ["/"]
    tls:
      enabled: false
      secretName: sproxy-tls
      certManager: false     # true → 自动注入 cert-manager.io/cluster-issuer 注解
      clusterIssuer: letsencrypt-prod
  ```
- 新模板 `templates/ingress.yaml`（networking.k8s.io/v1）：`{{- if .Values.ingress.enabled }}` 门控；`ingressClassName` 条件输出；hosts 循环（pathType: Prefix）；`tls.enabled` 时输出 tls 块（hosts + secretName）；`certManager` 时注入 annotation。
- Service 保持 ClusterIP 不动（Ingress 指向 Service）；`service.type` 仍可切 NodePort/LoadBalancer（文档说明）。
- 与 sproxy 内置 TLS 的关系（values 注释 + README）：Ingress 边缘终结 TLS 时后端走 HTTP → 建议关内置 tls（`tls.enabled: false`），避免双层 TLS。

## 数据流
helm install → values 渲染 → Ingress 资源（enabled 时）→ 集群 Ingress Controller 终结 TLS（secret/cert-manager）→ Service → Pod。

## 错误处理
- `tls.enabled=true` 且 secretName 空：`{{ required "ingress.tls.secretName 必填" .Values.ingress.tls.secretName }}` 渲染期 fail。
- `certManager=true` 且 clusterIssuer 空：同上 `required`。
- enabled=false：不渲染任何 Ingress（helm lint 通过）。

## 测试 + 变异点
- `deploy/sproxy-helm/tests/ingress_test.go`：CI 有 helm 时跑 `helm template --set ingress.enabled=true` 快照断言关键字段；无 helm 时 Go 测试直接解析 templates/ingress.yaml 断言门控。
  1. enabled=false → 无 `kind: Ingress`（变异：去掉 `{{- if }}` → 红）。
  2. tls.enabled=true → 输出 tls 块 + secretName（变异：漏 tls 块 → 红）。
  3. certManager=true → 含 cert-manager.io/cluster-issuer 注解（变异：漏注解 → 红）。
  4. secretName 空 + tls 开 → required 报错（变异：去 required → 红）。

## 片划分
- P1：values + ingress.yaml + required 校验 + 渲染测试。
- P2：README（边缘 TLS 与内置 TLS 互操作、cert-manager 安装步骤）+ chart version bump 说明。

## 风险与零回归保证
- 默认 enabled=false → helm template 输出与现 chart 完全一致（零回归，可用现有部署复测）。
- 不动 Service/Deployment/PVC；仅新增模板与 values 键。
- `required` 只影响显式开启 TLS 的渲染
