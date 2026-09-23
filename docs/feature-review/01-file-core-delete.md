# 审查：删除路径（POST /delete + checksum 匹配）

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

- **TOCTOU 加固（2026-09-17 quarantine）**：`DeleteFile`（`write_ops.go:1036-1059`）先把 rel 原子 rename 到 `quarRel = rel + ".deleting.<nano>"`，**基于 quarantine 内容**做 checksum 校验，匹配才删除——校验与删除之间并发写者只能替换原 rel，不影响被校验/删除的对象（窗口闭合）。错误路径全部 `atomicRenameRoot(quarRel, rel)` 恢复（不丢用户数据）。
- **checksum 门禁**：`X-File-Checksum` 必填（空 → 400 reasonChecksumMissing）；不匹配 → 恢复原路径 + 400（`write_ops.go:1090-1097`）。
- **幂等删除**：`AllowMissing=true`（批量族）时文件不存在 → Idempotent 成功（`idempotentMissingDelete`，重放安全）；单条 API false → 404。
- **并发互斥**：`fileLocks().Acquire(owner, rel)`（`write_ops.go:999-1006`）与 move/upload/version restore/分块 complete 共用锁池；批量族 SkipFileLock（历史语义：逐条立即给结果）。
- **配额释放**：删除后 `scope.ReleaseUsage(info.Size())`（owner Scope）+ `pool.ReleaseCommitted(info.Size())`（卷容量池，AD-7 双账本闭合）。
- **去重引用计数**：`ds.RemoveRef(rel, homeVol, cs)` 归零才删 inode + 释放配额；仍有引用只 unlink 目录项（`write_ops.go:1099-1122`）。
- **台账/索引/事件同步**：checksum store `Delete(rel)` + 索引 `remove` + `EventDelete` 事件（`write_ops.go:1124-1131`）。
- **审计**：成功/失败/denied 均留审计（含 checksum 不匹配 denied、文件不存在 error、幂等 success）；审计 Detail 不含 err.Error()（M-4：防绝对路径泄露）。
- **批量语义**：`BatchDelete` 逐条循环、单条失败不影响其余（`delete.go:141-160`）；`batchDeleteMessage` 按 HTTPError.Reason 稳定分派。
- **测试**：`delete_toctou_test.go`（TOCTOU 注入钩子）、`delete_test.go`、`rename_delete_contract_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（DeleteFile 全流程 + quarantine 机制 + 配额释放 + 去重引用）
- `go test -count=1 -timeout 120s -run 'TestDelete' ./pkg/files/...` → **ok**
