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
- 多副本：`replicaCount > 1` 时仅支持**只读面水平扩展**，写面单写主约束见下文「多副本写面限制声明」

### 多副本写面限制声明（只读面水平扩展 + 单写主）

> 11.6-④ 配套约束文档（与 11.6-③ 的 RollingUpdate/PDB 多副本不中断配套）：**「不中断」只承诺读面**；
> 写面在写主单点，写主滚动期间写不可用但读不中断。与 11.11 集群化写面唯一（single write master）语义一致。

**sproxy 多副本部署仅支持只读面水平扩展；写面只有一个写主（Write Leader），即序号为 0 的副本（statefulset/replica 0）。写请求应只发给写主副本；其余副本仅提供读。**

共享 PVC（默认 `ReadWriteOnce`）下多副本并发写会产生写冲突/数据分叉，因此多副本部署按下表拓扑约束：

| 请求面 | 路由 | 说明 |
|--------|------|------|
| 读请求（GET/HEAD 等） | Service/Ingress → 任意副本 | 只读面可水平扩展，副本 0..N 均可服务 |
| 写请求（POST/PUT/DELETE 等） | 负载均衡 → **仅写主副本（副本 0）** | 落 PVC（RWO 挂载在写主）；写面仅副本 0 单写主 |

**判定口径（写主 = 副本 0）**

- 单副本（`replicaCount: 1`）：唯一副本即写主，行为与现状完全一致（零回归，默认形态）。
- 多副本（`replicaCount > 1`）：**副本序号由编排层负责**——Deployment 无稳定序号，生产多副本场景请使用 `StatefulSet`（副本 0 = 写主，PVC 同序挂载），或由负载均衡器（Ingress/Service）把写请求路由到写主副本。Helm chart 本片不新增编排机制（仅声明语义）。

**Helm 配置指引**

- 默认单副本：`replicaCount: 1`（唯一副本即写主，无需额外配置）。
- 只读水平扩展：`replicaCount > 1` 后读请求可分发到任意副本；写请求必须由 Ingress/Service 规则或外部负载均衡定向到写主（副本 0）。写主故障（StatefulSet 滚动）时序号 0 的 PVC 挂载到新 Pod 0，写面恢复；滚动期间写短暂不可用但读不中断。
- 写请求误发到读副本：结果不被保证（取决于本地存储状态）——请确保负载均衡把写路由到写主；违背本声明并发写多个副本产生的数据分叉**不在支持范围内**。
