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
