# 部署指南（IaC 总览）

> 三套部署路径互补：Helm（Kubernetes 应用描述）→ Terraform（基础设施声明式管理）
> → Ansible（裸机/VM）。roadmap 11.10-⑦。

## 1. Helm（Kubernetes）

```bash
helm install sproxy ./deploy/sproxy-helm --set image.tag=v0.21.0
```

- **默认零行为变化**：`ingress.enabled=false` / `secret.create=false` / `hpa.enabled=false`
  （新增资源全部 `if enabled` 包裹，既有部署零 diff）。
- **Ingress**：`--set ingress.enabled=true --set 'ingress.hosts[0].host=files.example.com'`
  （TLS 可选：`ingress.tls.enabled=true` + `secretName` 或 cert-manager）。
- **Secret（敏感配置）**：`--set secret.create=true --set secret.apiKeys=user:key`
  以 `stringData` 注入 `SPROXY_API_KEYS` / `SPROXY_METRICS_TOKEN` / `SPROXY_TUNNEL_KEY`
  环境变量，不落 ConfigMap 明文。
- **HPA**：`--set hpa.enabled=true`（CPU 利用率扩缩容）。
- **PDB**：默认 `enabled=true`（minAvailable=1，多副本排水保护）。
- **滚动更新不中断**：默认 `strategy.type=RollingUpdate` + `rollingUpdate.maxUnavailable=0` /
  `maxSurge=1`（`values.yaml` 可覆盖）——新 Pod Ready 前不删旧 Pod，单副本升级也不出现
  「零就绪副本」窗口（单副本下行为与 Recreate 等价）。
- **探针分工**：`readinessProbe` 走 `/readyz`（依赖就绪才计入 Ready，升级真正等业务就绪）；
  `livenessProbe` 保持 `/healthz`（进程存活）。
- **`replicaCount=0` 与 PDB 冲突**：`pdb.enabled=true` 时模板 `required` 拒绝渲染（fail-closed）。

### 多副本写面限制（只读面水平扩展 + 单写主）

> 权威声明：**sproxy 多副本部署仅支持只读面水平扩展；写面只有一个写主（Write Leader）。**

- **只读面可水平扩展**：读请求（GET/list/stat/download 等）经 Service/Ingress 落到任意副本。
- **写面只认一个写主**：建议以 `replicaCount=1` 作为当前默认（唯一副本即写主，行为与单机一致）；
  若必须多副本，则**当前不做运行时强制**——写请求误发到读副本的后果不被保证（取决于本地存储状态），
  需部署方自行保证写请求只落到写主（`Deployment` 序号不稳定，需用 `StatefulSet` 等让「副本 0」
  稳定，再把写流量定向过去）。
- **PVC 限制**：chart 默认 `accessMode: ReadWriteOnce` ⇒ 多副本**不能同时挂同一卷**；多副本的前提是
  外部共享存储（外部卷/远端卷等），共享存储下的写冲突语义不由 helm 承担。
- **可观测**：`GET /api/mesh/status` 可见本节点角色/地址；写面排障以此为入口。
- 与「集群化写面唯一语义」一致：全局只允许一个写面（参见 `cluster.role=replica` 只读副本口径）。

## 2. Terraform（基础设施即代码）

```bash
cd deploy/terraform/examples
terraform init && terraform apply
```

- `modules/sproxy` 包装 `helm_release`：`image_tag` / `replica_count` / `ingress.enabled` /
  `secret.create` / `storage.size` 全部变量化。
- **Secret 值用 `sensitive = true` 标注**（tfstate 不明文展示）；从 TF vars/环境传入。
- 清理：`terraform destroy`（release 命名前缀统一）。

## 3. Ansible（裸机/VM）

```bash
ansible-playbook deploy/ansible/playbook.yml -i inventory
```

- `roles/sproxy`：渲染 config 模板 → systemd 或 docker 运行。
- `defaults/main.yml`：`sproxy_run_mode`（systemd|docker）/ 镜像 tag / 存储根 / 监听地址。
- 配置变更自动触发 `restart sproxy` handler。

## 4. 选择指南

| 场景 | 推荐 |
|------|------|
| Kubernetes 集群 | Helm（+ Terraform 管理 release） |
| 已有云基础设施 | Terraform module |
| 裸机/VM/边缘 | Ansible role |
| 开发/测试 | 直接 `make run` 或 docker run |
