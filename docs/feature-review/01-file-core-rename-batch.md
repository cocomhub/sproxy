# 审查：重命名/移动 + 批量操作

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] 批量 rename 单条失败不中断整批（设计如此，文档缺失）
- **位置**：`pkg/files/rename.go:55-72`（processBatchRenameItem）
- **问题**：批量 rename 逐条调用 `RenameFile`，单条失败（如源不存在/checksum 不匹配/409 锁冲突）只在该条结果标记失败，**整批继续**。这是历史语义（逐字保留），但 API 文档未显式说明「部分成功」语义，调用方可能误以为全部成功才提交。
- **建议**：api.md 批量端点补充「单条失败不影响其余、响应 results 逐条状态」说明。

## 通过项（无问题面）

- **原子性**：`RenameFile`（`write_ops.go:673-767`）from/to 两把非阻塞锁（FileLocks，同 rel 池）闭合「目标检查 → 原子改名」TOCTOU 窗口；`atomicRenameRoot` 原子替换。
- **checksum 门禁**：`X-File-Checksum` 必填（空 → 400）；`verifyFileWithChecksumRoot` 校验源（`write_ops.go:855-859`），不符 → 400 不 rename。
- **目标唯一性（AD-4）**：目标在其它卷已存在 → 409（`write_ops.go:737-741`）；同卷目标存在 → 409（`renameInHome:849-854`）——防跨卷双份。
- **配额对称转移**：跨 bucket_limits 子目录时 `toScope.TryReserve(size)` → 原子 Rename → `fromScope.ReleaseUsage(size)`（`write_ops.go:870-899`）；Rename 失败归还目标预留；同键零操作。
- **台账/索引同步**：`cs.Rename(fromRel, toRel)` + `index.rename`（`write_ops.go:890-895`）。
- **同源同目标短路**：`from == to` → NoOp 成功（`write_ops.go:680-682`），批量族原样透传。
- **审计**：rename 成功/denied（目标存在/checksum 不匹配）/error 均留痕；`origin=auditOriginBatch` 标记批量来源。
- **批量删除语义**：`BatchDelete` 逐条 + `AllowMissing` 幂等 + `SkipFileLock`（历史：不因并发上传整批 409）；`batchDeleteMessage` 按 Reason 分派。
- **路径安全**：`validateRenamePaths`（from/to 双向 ValidateFilePath）+ `resolveRenamePaths`（UserRel 双向）。
- **测试**：`rename_test.go`、`rename_lock_test.go`、`rename_delete_contract_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（RenameFile 全流程 + 锁 + 配额转移 + 批量聚合）
- `go test -count=1 -timeout 120s -run 'TestRename|TestBatch' ./pkg/files/... ./pkg/server/...` → **ok**
