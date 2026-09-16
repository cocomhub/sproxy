# Task 1 报告：读 move 实现 + rebalance 设计

## 关键发现（moveVolumeHandler 原子语义，volumes_api.go:83-316）

1. **ACL 校验**：`fv.Authorize(owner)` / `tv.Authorize(owner)`，不在视图 → 403（不泄卷存在性）。
2. **uploadingFiles 锁**：`h.uploadingFiles.LoadOrStore(owner+"\x00"+rel, uploadingLockMove)`——与 upload/move 同键池，后到者 409。`defer h.uploadingFiles.Delete(upKey)`。
3. **源存在性**：`fromTnt := h.volumeTenant(fromVol, owner)` → `fromRoot.Stat(rel)`；IsNotExist → 404；IsDir → 400。
4. **同卷 no-op**：`fromVol == toVol` → 200 成功（幂等）。
5. **目标唯一性（AD-4）**：`checkMoveTargetUnique(owner, rel, fromVol, toVol)` 视图内其它卷已有同 rel → 409。
6. **双预留（AD-7）**：`scope := h.quotaScopeFor(owner, rel)` → `scope.TryReserve(size)`（owner 全局）+ `toPool.TryReserve(size)`（目标卷池）；失败回滚已预留（`scopeRes.Release()`）。
7. **流式复制**：`crossVolumeCopy(ctx, fromRoot, toTnt.Root(), rel, rel)` —— 临时文件 + fsync + 原子 rename，返回 written；`written != size` → fail-closed 回滚（TOCTOU 闭合）。
8. **删源三分支**：
   - `removeMovedSource(fromRoot, rel)` 成功 → 双 commit（`scopeRes.Commit(written)` / `poolRes.Commit(written)`）+ from 侧释放（`scope.ReleaseUsage(written)` / `fromPool.ReleaseCommitted(written)`）；
   - `errors.Is(rmErr, os.ErrNotExist)`（并发 delete 已删源）→ **只 commit to 侧**、立即 return（绝不 from 侧再释放）；
   - 其它错误 → 回滚 to 侧（删目标 + Release 预留），源保留。
9. **审计**：`h.RecordAudit(ctx, AuditEvent{...})`。

## rebalance 骨架（设计）

```go
// rebalanceVolumeHandler 处理 POST /api/volumes/rebalance?from_volume&to_volume&max_bytes。
// 从 from 卷 user 桶按大小降序列出文件，逐文件复用 move 语义（ACL/锁/双预留/流式复制/
// fsync/原子 rename/双 commit-release）迁到 to 卷，直到 max_bytes 用尽或无可迁文件。
// 返回 {success, moved, bytes_moved, remaining}。
//
// 实现要点：
// 1. ACL：from/to 都在 owner 视图（403 同 move）。
// 2. from==to → no-op 成功（moved=0）。
// 3. 列出 from 卷 user 桶全部文件（root.ReadDir(userRel) + filepath.WalkDir 递归）按 size 降序。
// 4. 逐文件：max_bytes 剩余足够才迁（不够跳过该文件继续看更小的）；调 move 核心辅助
//    moveFileBetweenVolumes（提取自 moveVolumeHandler，含锁/双预留/复制/删源/commit-release）。
//    单文件失败（409 目标已存在/配额不足/被并发迁走）→ 跳过该文件继续（rebalance 是尽力而为）。
// 5. remaining = from 池 Usage（迁移后）。
//
// 锁：moveFileBetweenVolumes 内部按同 rel 取 uploadingFiles 锁（与 move 共用）——并发
// rebalance × move × upload 同文件串行化，恰一成功其余 409/404。
```

## Task 1 无代码改动（只读任务）

任务 1 无提交（纯设计输出，含在本报告）。
