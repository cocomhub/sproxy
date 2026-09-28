# 批次 3 审查：多卷

> 本批 2 路并发对抗审查：R3.1 本地多卷 + 卷操作 / R3.2 外部后端 + 用户卷。
> 基线：master `e428acbe`。产出文件 `03-multivolume-*.md`。

## 审查目标功能（roadmap 3.1）

- **R3.1 本地多卷**：`volumes[]` 配置（`pkg/volume` 纯域 + `pkg/server/volumes.go` 装配）；
  每卷独立物理根 + `LAYOUT_VERSION` 校验 + 容量池（`pkg/quota.Pool`）；placement `prefer-default`/`spread`；
  卷 ACL（deny/allow）；meta 单点权威在默认卷（checksums/凭据/任务状态）。
  - 卷操作：`POST /api/volumes/move`（跨卷流式复制原子迁移）、`rebalance`（大小降序逐文件迁移）、
    `GET /api/volumes`（per-owner 视图）；sclient `volumes`/`--volume`/`mv --to-volume`。
- **R3.2 外部后端框架**：`pkg/volume/registry.RegisterBackend(type, factory)` 可插拔；
  `GET /api/backends` 动态列出；已注册 `baidupcs`/`webdav`/`s3`。
  - 用户自有卷：per-owner 网盘盘（`POST/GET/DELETE /api/volumes/user`，`<owner>/meta/volume/*.json`
    原子持久化 + 重启扫描恢复）；同步任务 `remote.volume` 寻址，跨用户 404 防枚举。
  - 容量账本：外部卷容量 = 本系统可用限额（`UserVolume.Capacity`），backend 级 Total/Used 查询。

## 关键文件

- R3.1：pkg/volume/*.go、pkg/server/volumes.go、pkg/server/volume_move.go（move/rebalance）、
  pkg/quota/pool.go（容量池）
- R3.2：pkg/volume/registry/*.go（RegisterBackend/ExternalBackend/Presigner/Stats/Usage/HealthProbe）、
  pkg/server/backends_api.go（/api/backends）、pkg/server/user_volumes.go（用户卷）、
  pkg/volume/federated/*.go（federated 后端）

## 审查维度与关注点

### 正确性
- move/rebalance 跨卷迁移原子性（流式复制中断、源/目标一致性、checksum 迁移）
- placement 语义（prefer-default/spread 在卷满时行为）；卷 ACL deny/allow 判定顺序
- 容量池与 quota.Scope 的账本一致性（多卷共享配额？）

### 安全性（本批重点）
- 外部后端：presigned URL 泄漏、路径穿越到外部后端、认证凭据存储
- 用户卷：跨用户访问控制（404 防枚举是否全路径）、用户卷与共享卷的权限边界
- 卷 move 的 owner 边界（能否把别的 owner 文件移走？）

### 可用性 / 可维护性
- 卷操作错误语义（卷不存在/容量不足/迁移中断恢复）
- 外部后端不可达时的降级（HealthProbe/Stats 失败是否 fail-open？）
- 测试覆盖（move/rebalance/后端注册/用户卷 CRUD）

## 输出格式

每路审查产出 `03-multivolume-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
