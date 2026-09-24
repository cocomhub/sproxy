# 多副本不中断（Helm）设计

> 批次：D-1（11.6-③）｜状态：草案｜日期：2026-09-24

## 背景 / 目标
- 现状（deploy/sproxy-helm/）：`deployment.yaml` **无 `strategy` 段**（默认 Recreate：更新先删旧 Pod 再建新 Pod，升级期间中断）；`replicaCount: 1`；livenessProbe 与 readinessProbe 都打 `/healthz`。
- 目标：让 `helm upgrade` / 滚动发布**不中断**：
  1. `strategy: RollingUpdate { maxUnavailable: 0, maxSurge: 1 }`——旧 Pod 全部 Ready 前不删；
  2. PodDisruptionBudget `minAvailable: 1`——自愿中断（节点排水/`kubectl drain`）也不全灭；
  3. readinessProbe 改 `/readyz`——容器起但依赖（UploadStore 健康）未就绪时不被计入 Ready，滚动才真正等业务就绪；livenessProbe 保留 `/healthz`（进程存活语义，语义与 /readyz 解耦，避免「依赖抖动杀 Pod」）。

## 组件与接口（Helm chart 内）
- `deployment.yaml`：`spec.strategy` 段 + readinessProbe `path: /readyz`（livenessProbe 保持 `/healthz`）。
- 新增 `templates/pdb.yaml`：`apiVersion: policy/v1`、`kind: PodDisruptionBudget`、`spec.minAvailable: {{ .Values.pdb.minAvailable }}`（默认 1）、`selector` 复用 deployment 的 `app.kubernetes.io/name/instance` 标签（与模板一致，避免 selector 漂移）。
- `values.yaml`：新增 `strategy`（`rollingUpdate.maxUnavailable: 0` / `maxSurge: 1`，允许覆盖）与 `pdb.minAvailable: 1`（`enabled: true` 开关；replicaCount=1 时 minAvailable=1 恒成立，语义安全）。
- Service 不需要改动（ClusterIP + Deployment selector 天然负载均衡到多副本）。

## 数据流（滚动升级时序）
1. `helm upgrade` → 新 ReplicaSet 建新 Pod（maxSurge=1：多起 1 个）。
2. 新 Pod `livenessProbe /healthz`（进程存活）→ `readinessProbe /readyz`（依赖就绪）→ Ready。
3. `maxUnavailable: 0` → 旧 Pod 保持服务直至新 Pod Ready；随后旧 Pod 滚动终止（优雅关闭路径 SIGTERM → handleSignalShutdown drain）。
4. 终态：全部新副本 Ready，全程至少 1 个就绪副本对外服务（无中断）。
5. 节点排水：PDB 拦 `drain` 直到其余副本 Ready（minAvailable: 1）。

## 错误处理
- 新副本永不 Ready（依赖故障）→ RollingUpdate 挂起（不删旧 Pod），有 `progressDeadlineSeconds` 可配（默认不配 = 无限等待，文档建议配 600s 便于回滚判定）；回滚：`helm rollback`。
- PDB 阻止 drain 超时：kubectl 强制 eviction 会 `--force` 绕 PDB（运维显式动作，文档声明属预期）。
- replicaCount=0（禁用）：PDB minAvailable=1 与 0 副本冲突 → 校验：`pdb.enabled && replicaCount==0` 时模板报错（fail-closed）或 helm 断言提示。

## 测试 + 变异点
- `helm template` 断言（go 测试或 CI 脚本，**变异命中**）：
  1. deployment 含 `strategy.type: RollingUpdate` 且 `maxUnavailable == 0`——**变异**：删 strategy 段 → 红；
  2. readinessProbe path == `/readyz`、livenessProbe path == `/healthz`——**变异**：readiness 误用 /healthz → 红；
  3. PDB 存在且 `minAvailable == 1`、selector 与 deployment 标签一致——**变异**：删 PDB / 改 selector → 红；
  4. `pdb.enabled=false` 时不渲染 PDB——**变异**：条件反了 → 红；
  5. `replicaCount=0 && pdb.enabled` 渲染报错——**变异**：放行 → 红。
- 集成（可选手动）：kind/minikube 上 `helm upgrade` 观察 Pod 滚动（旧 Pod 保持 Ready 至新 Pod Ready）；`kubectl drain` 验证 PDB 拦截。

## 片划分
- P1：deployment.yaml strategy 段 + readinessProbe 改 /readyz + values 新增字段。
- P2：pdb.yaml 模板 + values.pdb 段 + 条件渲染。
- P3：helm template 断言测试（上述 5 条，变异验证）+ 文档（CHANGELOG/设计说明：写面唯一约束见 11.6-④ 文档）。

## 风险与零回归保证
- 零回归：单副本场景下 RollingUpdate maxUnavailable=0/maxSurge=1 与 Recreate 行为等价（就 1 个 Pod，先建新的再删旧的）；/readyz 语义是 /healthz 的超集（依赖健康时返回相同 200 OK）。
- 多副本写面风险：**多副本共享一个 PVC（ReadWriteOnce）**——`replicaCount > 1` 时写面竞争，读面可水平扩展。约束见 11.6-④ 文档（多副本只读面 + 单写主）；values 注释与文档声明 `replicaCount > 1` 仅用于只读水平扩展。
- PVC accessMode ReadWriteOnce 天然限制多副本同时挂载——已记录为文档声明（写面唯一副本 0），不在本片解决。
- 若用户自管 RWO 之外的共享存储，多副本 read 面经 `/api/mesh/status` 可观测（现状已有），文档补充说明。
