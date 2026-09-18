# sproxy 部署指南

sproxy 提供两种部署工件：**Docker Compose**（单机快速部署）与 **Helm chart**（Kubernetes）。

## 镜像

- 镜像构建：`ghcr.io/cocomhub/sproxy`（GoReleaser 发布时 `dockers_v2` 自动构建）
- tag：`latest` 或版本号（如 `v0.15.0`）
- 镜像内：`sproxy` + `sclient` 双二进制，非 root 用户（`sproxy`），数据根 `/app/storage`

## Docker Compose（单机）

```bash
# 1. 准备配置（从模板拷贝，按需修改）
cp config.example.yaml deploy/sproxy.yaml

# 2. 启动
docker compose up -d

# 3. 验证
curl http://127.0.0.1:18083/healthz    # → OK
curl http://127.0.0.1:18083/version    # → Version: ...

# 4. 日志
docker compose logs -f sproxy

# 5. 停止
docker compose down          # 保留数据卷
docker compose down -v       # 删除数据卷（谨慎）
```

### 配置说明

- 配置文件挂载为 `deploy/sproxy.yaml`（只读）——环境变量（`SPROXY_*`）优先级高于配置文件
- 数据卷：`sproxy-storage`（命名卷，持久化）
- 端口：`18083`（HTTP/HTTPS 主端口，含 hub/federation 端点）
- 生产建议：反向代理 TLS（或配置证书）、资源限制（`deploy.resources`）

## Helm chart（Kubernetes）

```bash
# 1. 安装（默认使用 env 配置）
helm install sproxy ./deploy/sproxy-helm

# 2. 自定义配置（挂载 config.yaml）
helm install sproxy ./deploy/sproxy-helm \
  --set config="$(cat deploy/sproxy.yaml)"

# 3. 指定镜像版本
helm install sproxy ./deploy/sproxy-helm \
  --set image.tag=v0.15.0

# 4. 验证
kubectl get pods -l app.kubernetes.io/name=sproxy
kubectl port-forward svc/sproxy-sproxy 18083:18083

# 5. 升级
helm upgrade sproxy ./deploy/sproxy-helm --set image.tag=v0.16.0

# 6. 卸载
helm uninstall sproxy
```

### Chart 组件

| 组件 | 说明 |
|------|------|
| Deployment | 单副本（`replicaCount` 可调），非 root 安全上下文，探针 `/healthz` |
| Service | ClusterIP（默认）暴露 18083 |
| PVC | 持久化存储（默认 10Gi，`persistence.size` 可调） |
| ConfigMap | 可选配置挂载（`values.config` 非空时） |
| 环境变量 | `values.env` 键值对注入（优先级 CLI > env > config） |

### 生产建议

- 存储：为 PVC 配置合适的 StorageClass（`persistence.storageClass`）
- 资源：`values.resources` 设置请求/限制
- 升级：`helm upgrade`（sproxy 无状态迁移负担——多租户存储布局在 PVC 上，升级保留）
- 多副本：`replicaCount > 1` 时注意共享存储的并发写（默认单副本）
