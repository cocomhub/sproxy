# 批次 4 审查：云同步

> 本批 2 路并发对抗审查：R4.1 同步引擎 / R4.2 跨节点授权 + 任务 API。
> 基线：master `e428acbe`。产出文件 `04-sync-*.md`。

## 审查目标功能（roadmap 4.1）

- **R4.1 同步引擎**：`sclient sync push/pull`；服务端 `SyncManager`（`pkg/syncmgr`）托管执行；
  `--remote` 指 `sync_remotes[]` 配置的远端；任务状态机 pending/syncing/completed/failed/retrying/
  cancelled，带超时与重试（`sync.max_retries`）。
  - 增量能力：文件级增量（`pkg/sync` diff/engine/filter）、glob include/exclude、`--recursive`、
    `--follow-symlinks`、`--sync-empty-dirs`；冲突策略 `skip|overwrite|lww|conflict-rename`。
  - 载体分组：`sync_remotes[].kind` 支持 `direct`（HTTP 直连，url+AK/SK）与 `mesh`
    （mesh 隧道：node/volume/peer_pins/transport）；`transport` 选路矩阵 relay/auto/webrtc。
- **R4.2 跨节点授权 + B 侧形态 + 任务 API**：
  - 跨节点授权：`mesh_readers` 卷 ACL 授权（owner 维度，`GET /api/mesh/acl` 只读视图）；
    出口拨号策略（loopback/私网拒绝，除非宣告服务地址）。
  - B 侧形态：进程内 mesh 角色（推荐）或独立 `remote_read`/`remote_write` 面宣告服务。
  - 任务 API：`/api/sync/tasks` CRUD + cancel + delete（被活跃任务引用的用户卷 409 保护）；
    用户卷寻址（`remote.volume`，跨用户 404 防枚举）；Web UI 任务面板。

## 关键文件

- R4.1：pkg/syncmgr/*.go（任务状态机）、pkg/sync/*.go（diff/engine/filter）、
  pkg/server/sync_handler.go、cmd/sclient/sync.go
- R4.2：pkg/server/mesh_acl*.go（mesh_readers）、pkg/server/remote_read.go + remote_write.go
  （B 侧形态）、pkg/server/sync_task*.go（任务 CRUD）、pkg/tunnel/mesh/*.go（RunNode）

## 审查维度与关注点

### 正确性
- 任务状态机迁移合法性（pending→syncing→completed/failed/retrying/cancelled 全路径）
- 增量 diff 正确性（mtime/size/checksum 判定）；冲突策略各语义实现
- 取消/重试的幂等性；超时处理

### 安全性（本批重点）
- mesh_readers ACL 授权校验点覆盖（上传/下载/删除是否都校验）
- 出口拨号策略（loopback/私网拒绝能否被绕过——如 DNS 重绑定/IPv6 映射）
- 用户卷跨用户寻址（404 防枚举是否全路径）
- 载体凭据（AK/SK 存储与传输）

### 可用性 / 可维护性
- 任务 CRUD 错误语义；Web UI 面板数据来源
- 同步引擎可测试性（synctest/virtual time 使用）；测试覆盖
- 文档同步（cli.md/api.md）

## 输出格式

每路审查产出 `04-sync-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
