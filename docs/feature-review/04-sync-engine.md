# 审查：同步引擎（任务状态机/增量/冲突策略/载体分组）

- **批次**：4
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] 任务状态机无显式迁移合法性校验（状态直接赋值）
- **位置**：`pkg/syncmgr/manager.go`（CreateTask/CancelTask/executeSync 状态赋值）
- **问题**：状态机迁移（pending→syncing→completed/failed/retrying/cancelled）由各路径直接 `t.Status = ...` 赋值，无集中式 `validateTransition(from, to)` 守卫。当前调用点均正确（写锁内变更 + cancel 只允许 active 状态），但未来新增状态路径可能引入非法迁移。
- **建议**：可选——加 `transitionTask(t, to)` helper 集中校验；低风险（当前路径已逐条核对）。

## 通过项（无问题面）

- **任务状态机**：pending→syncing（SubmitAndStart 写锁内复查 pending + 置 running，**闭合「检查→启动」竞态**，防双 goroutine 并发执行）→completed/failed/retrying/cancelled；retry（`retry.go` RetryFiles 按文件重试 + waitTaskTerminal + backfillRetryResults）。
- **去重**：CreateTask 写锁内去重（同 owner 可见 + 同参 + active 状态）→ 返回既有任务；**跨 owner 不吸收**（防泄露归属与进度）。
- **配额预留**：pull 1GiB 占位预留（best-effort，不足降级 0 由 reconcile 强制）；Cancel/Delete 释放预留（锁内归零防二次释放）。
- **IDOR 防护**：Get/List/Cancel/Delete 均 `ownerVisible(t.Owner, owner)` 过滤（跨 owner 404/ErrNotFound 防枚举）。
- **深拷贝**：Get 返回深拷贝（Include/Exclude/Results/Carriers map 深拷——防后台回填并发读写）。
- **扇出**：多节点 Remotes → 父任务聚合视图 + 各子任务独立执行/独立失败。
- **持久化**：saveTask（取消/删除显式持久化防状态丢失）+ 重启恢复。
- **增量**：`pkg/sync` diff/engine/filter + 块级增量 v2（blockdiff/merge3）+ 冲突策略 skip/overwrite/lww/conflict-rename。
- **测试**：`syncmgr` 全包测试 + `pkg/sync` 测试；`go test` 全绿。

## 验证方式

- 源码逐路径审查（状态机 + 去重 + 配额 + IDOR + 扇出）
- `go test -count=1 -timeout 180s ./pkg/syncmgr/... ./pkg/sync/...` → **ok**
