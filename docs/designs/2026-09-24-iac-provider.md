# IaC provider（11.10-⑦）

## 背景/目标
- 现状：`deploy/sproxy-helm/` 只有基础 chart（Deployment + Service + PVC + ConfigMap + values.yaml），**无 Ingress、无 Secret、无 HPA/PDB/ServiceAccount、无编排态导出**；没有任何 Terraform/Ansible 层。
- 目标：① chart 补 Ingress（+ TLS/annotations）+ Secret（隧道密钥/API key/metrics token）+ 可选 HPA/PDB，**默认全关零行为变化**；② 新增 Terraform provider（或 module）与 Ansible role 管理部署/配置，与 Helm 互补（IaC 描述基础设施，chart 描述应用）。

## 组件与接口
- **Helm 增强**（deploy/sproxy-helm/）：
  - `templates/ingress.yaml`：`{{- if .Values.ingress.enabled }}`；host/path（默认 `/`）、`ingressClassName`、`annotations`、`tls[].hosts/secretName`。
  - `templates/secret.yaml`：`{{- if .Values.secret.create }}`——`tunnel_key`/`api_keys`/`metrics_token`（`values.yaml` 或 `--set` 注入；`stringData`，K8s 侧落 Secret，不用明文进 ConfigMap）。
  - `templates/hpa.yaml`/`templates/pdb.yaml`：默认 `enabled: false`。
  - `values.yaml`：新增 `ingress:`/`secret:`/`hpa:`/`pdb:` 四段 + 注释。
  - `templates/NOTES.txt`（新增）：打印访问 URL / 升级提示 / 默认未开 Ingress 的提示。
- **Terraform**（deploy/terraform/，模块式）：
  - `modules/sproxy/`：`main.tf`（helm_release 资源包装 values；`variables.tf` 暴露 image.tag、replicaCount、ingress、secret、storage 等）；`outputs.tf`（service endpoint、release name）。
  - 根 `examples/`：最小示例（本地 kind/minikube 可跑）。
- **Ansible**（deploy/ansible/）：
  - `roles/sproxy/`：`tasks/main.yml`（docker 或 systemd 容器运行，config 模板渲染 → 启动/restart）；`defaults/main.yml`（镜像 tag、存储根、监听地址）；`handlers/`（restart sproxy）；`templates/sproxy.yaml.j2`（server config 模板）。

## 数据流
Terraform：`terraform apply` → helm_release → chart 渲染（含 Ingress/Secret）→ K8s 创建；Secret 值从 TF vars/环境传入（不进 tfstate 明文？用 `sensitive` 标注，文档说明）。Ansible：inventory → playbook → 目标机拉镜像 → 模板化 config → systemd 起服务。

## 错误处理
- Helm 渲染失败：`helm template` 在 CI 验证（新增 `.github/workflows/helm-lint.yml` 或并入现有 CI 的 notest/check 类 job）；`values.yaml` 缺段 → chart 默认值兜底。
- TF：`helm_release` 失败 → `terraform destroy` 可清理（资源命名前缀统一）；敏感变量缺失 → 明确报错（必填变量无默认）。
- Ansible：服务启动失败 → handler 不重启 + 任务失败可见；config 模板渲染（Jinja2 严格模式防 typo）。

## 测试+变异点
- **CI 必须可跑且快**：新增 `make helm-lint`（`helm lint`/`helm template` 渲染断言：默认渲染产物不含 Ingress/Secret 对象；`ingress.enabled=true` 时含）；`terraform validate`/`terraform fmt --check`（无 provider 依赖，纯静态可入 CI）；`ansible-playbook --syntax-check`（纯语法可入 CI）。
- 变异验证：① ingress 开关条件写反（`if not`）→ 默认渲染含 Ingress 测试红；② secret 模板漏 `if` → 默认渲染含 Secret 测试红；③ TF 变量默认值改错 → `terraform validate` 失败（或输出断言测试红）。

## 片划分
- 片1：chart Ingress/Secret/HPA/PDB/NOTES + values + helm 渲染测试（纯 deploy/ + CI job）。
- 片2：Terraform module + example + validate 静态测试。
- 片3：Ansible role + syntax-check + docs/deploy.md（IaC 总览 + 各层选择指南）。

## 风险与零回归
- chart 全部新资源 `if enabled` 默认 false → 既有 `helm install` 零 diff（golden 渲染对比旧 chart）；不改 Deployment/PVC 既有字段；Terraform/Ansible 是新增目录不进 chart 渲染路径；CI 只加静态检查，不部署真集群（无网络/无凭据需求）。
