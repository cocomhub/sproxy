# 审查：分块上传会话（POST /upload/init|chunk|status|complete）

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] upload_id 熵依赖客户端派生（同文件重试同 id 是特性）
- **位置**：`pkg/files/chunked_upload.go:193-200`（UploadInit 校验 upload_id）
- **问题**：`upload_id` 完全由客户端提供（SDK 按 filename|size|mtime|checksum 确定性派生），服务端只做 `ValidSegmentName` 段名校验（防路径穿越）。**可预测性不是漏洞**：per-tenant chunk 桶物理隔离（跨租户同 id 不可见），且会话归属 owner 由请求认证决定；同文件重试同 id 是续传特性。仅提示：恶意客户端可用**任意** id 创建会话（占用磁盘 Truncate(TotalSize)）——受 quota Scope 预留 + maxTotalChunks 上界 + TTL 回收约束，DoS 面有界。
- **建议**：可选——init 时校验 `TotalSize ≤ 配额可用` 已在 routeUpload 双预留覆盖；无需额外熵要求。

## 通过项（无问题面）

- **会话状态机**：init→chunk→complete 全路径 + 异常路径（缺块 400 + MissingChunks 列表、重复块幂等 200、乱序 seek 直写、超时 TTL 清理）。complete 幂等（Completed 直接 200）。
- **合并中屏障（C-3）**：`BeginComplete` 置 Completing → UploadChunk 立即 409 ShouldRetry（瞬态窗口 SDK 退避重试）；`EndComplete` defer 无条件清除（防「Completed=false ∧ Completing=true」死锁窗口）。
- **内存放大防护**：`maxTotalChunks = 1<<16`（65536）服务端上界 + `validateChunkPlan`（乘法溢出 + 覆盖性 + 裁剪后重校验）——单请求无法让服务端为会话分配 GiB 级 bitmap（实测 2^24 会 ~528 MiB，已拦）。
- **配额**：init 双预留（owner Scope Reservation + 卷容量池 PoolRes）；complete Commit(total) + ReleaseUsage(prev) 显式对账（I1）；失败/取消/过期释放预留（DeleteSession/CleanupExpired 身份闸门内归还）；P5 回退（quota 未装配）StorageMgrReserved 只对**未完成**会话释放（C-4 防容量账少算）。
- **身份闸门（C-7/RV9）**：`cleanupExpiredArtifacts`/`cleanupSessionIfCurrent`/`abortInitOrphanRollback` 全部「身份判定 + 删除」同一 us.mu 临界区——同 id 被新会话接管时不误删新会话产物。
- **临时文件安全（F-3）**：`deleteSessionArtifactsAt` 只删 `IsInflightTempNameFor`（形态 + 内嵌 upload_id 归属）的路径——被篡改 session.json 写 `user/important.txt` 不删正式文件（实测复现已修）。
- **恢复**：`recoverSessions` 重启扫描——损坏/缺失 session.json 回收目录（C-5 防孤儿）、过期就地回收、已完成保留供查询；预留句柄 json:"-" 重启为 nil（上游扫描对账）。
- **持久化原子性**：writeSessionJSON 写锁串行 + tmp+rename（Windows 删除目标重试 + WriteFile 兜底）；persistSession 持 persistMu 深拷贝快照（RV10 防旧快照覆盖新快照）。
- **并发安全**：us.mu 保护 sessions map + MarkChunkReceived；LockChunkIO（多分片并发写）+ LockChunkMerge（complete 排他读）；copySession 深拷贝隔离并发读写。
- **块校验**：每块 SHA-256（`UploadChunk:705` 不匹配 400 ShouldRetry）；complete 全文件校验（`prepareMergedTemp`）失败逐分片 seek 定位 mismatch_chunks（I-2）+ ClearChunksReceived 落盘。
- **测试**：24 个 chunked 测试文件（状态机/孤儿/接管/屏障/持久化序/回收/TOCTOU）；`go test -run 'TestChunk' ./pkg/files/...` → ok（0.77s）。

## 验证方式

- 源码逐路径审查（UploadStore 全生命周期：init 身份门控发布 → chunk seek 直写 → complete 合并 → TTL/取消/接管清理）
- `go test -count=1 -timeout 180s -run 'TestChunk' ./pkg/files/...` → **ok**
- `go test -count=1 -timeout 180s -run 'TestChunked|TestComplete' ./pkg/files/...` → **ok**
