# 审查：本地多卷 + 卷操作（move/rebalance）

- **批次**：3
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### 卷装配（`pkg/volume/volume.go` + `pkg/server/volumes.go`）
- **Volume 模型**：Name/Type/RootDir/Capacity/ACL/Extra/MirrorOf/Mirrors/Tier；ACL deny/allow（`Authorize`）；placement `prefer-default`/`spread`（`OrderCandidates` 按容量剩余排序）。
- **meta 单点权威**：checksums/凭据/任务状态在默认卷（AD-6）。

### 卷操作（`pkg/server/volumes_api.go`）
- **move 原子性**（`moveFileBetweenVolumes:306-490`）：
  - 并发防护：`uploadingFiles.LoadOrStore(upKey, uploadingLockMove)`（同 rel 串行化 move/upload/rebalance）；
  - 源存在性锁内判定；同卷 no-op 幂等；
  - **目标唯一性（AD-4）**：`checkMoveTargetUnique` 视图内其它卷同 rel → 409；
  - **双预留**：owner 全局 Scope + 目标卷池 TryReserve（任一失败回滚）；
  - **流式复制**：`crossVolumeCopy`（temp + fsync + 原子 rename）+ **TOCTOU 纵深防御**：`written != size` → 删目标 + 双 Release 回滚（源不动的 fail-closed）；
  - **删源三分支**：成功 → 双 commit + from 释放；`IsNotExist`（并发 delete 已释放 from 账本）→ **只 commit to 侧**（防 PR-D F1 双欠计）；其它错误 → 回滚 to 侧。
- **rebalance**（`:202-295`）：大小降序逐文件复用 move 原子语义；max_bytes 用尽即停；单文件失败跳过（尽力而为）；from==to no-op；进度 /metrics 可观测 + defer 清理。
- **ACL 视图**：`volumeAllowedFor` from/to 都在 owner 视图内（403 fail-closed 不泄卷存在性）。
- **配额双账本**：move 的 to 侧 reserve-then-commit + from 侧 ReleaseCommitted（owner 全局 + 卷池各释放一次）。
- **审计**：move/rebalance 成功/失败/denied 均留痕。

### 测试
- `volumes_api_test.go`、`volume_move_test.go` 存在（含 TOCTOU/PR-D F1 语义）；`go test` 全绿。

## 验证方式

- 源码逐路径审查（move 6 步 + 删源三分支 + rebalance 循环）
- `go test -count=1 -timeout 180s -run 'TestVolume|TestMove|TestRebalance' ./pkg/server/... ./pkg/volume/...` → **ok**
