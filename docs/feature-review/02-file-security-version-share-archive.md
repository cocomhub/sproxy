# 审查：版本管理 + 分享 + 归档

- **批次**：2
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 1 / P3 1

## 发现清单

### [P2] 分享 token 容量满时按创建时间淘汰最旧 10%（Evict 语义需文档化）
- **位置**：`pkg/server/share.go:283-330`（Create 满容量淘汰）
- **问题**：`maxShareEntries` 满时先清过期，仍满则**按创建时间淘汰最旧 10%**（`evictCount = maxShareEntries/10`）——**活跃未过期**的分享会被静默删除。这是容量保护设计，但调用方/用户可能意外丢失分享链接。
- **建议**：文档化（api.md 分享节）+ 可选：淘汰时记审计。

### [P3] 归档 tar 条目名直接采用 rel（无显式 tar path 归一）
- **位置**：`pkg/server/archive.go:246-249`（header.Name = filepath.ToSlash(tarRel)）
- **问题**：tar 条目名 = 用户 rel 的 ToSlash——用户文件本身已过 ValidateFilePath + UserRel 校验（无 `..`/绝对路径），故 tar 条目无路径穿越面；但目录递归时 `path.Join(rel, entry.Name())` 的 entry.Name() 来自 ReadDir（磁盘条目名，可能含 `..`？）。**核实**：os.Root.ReadDir 返回的 DirEntry.Name() 是单段文件名（无 `/`），`path.Join` 不会引入 `..`。且 root.Open 防逃逸。**无实际风险**，仅记录防御推理。
- **结论**：无问题面，归档 tar 路径安全（TOCTOU os.SameFile 交叉验证 + 符号链接拒绝 + 100MB 单文件上限 + 深度 100 上限）。

## 通过项（无问题面）

### 版本管理（`pkg/server/version.go`）
- **生命周期**：list（跨卷合并 CollectVersionEntries + checksum 台账）/ restore（文件级互斥 + 跨卷定位 + SaveVersion 备份 + reserve-then-commit 配额 + 卷池双账本）/ delete / GC（gcAllExpiredVersionsPass 整仓保留期清理 + 写入路径上限截断）。
- **restore 配额**：TryReserve(版本大小) → 拷贝 → Adjust/Commit（`version.go:205-260`）——缺失配额可反复 restore 突破上限的面已闭合。
- **AD-4 防双份**：目标卷 = user 文件当前所在卷；版本字节不迁移。
- **TOCTOU**：deleteVersion 带 quarantine 手法（`version_delete_toctou_test.go`）。

### 分享（`pkg/server/share.go`）
- **token 熵**：16B crypto/rand + hex（32 字符）——不可猜测。
- **持久化**：`persistWrite`（原子写 `<root>/anonymous/meta/share/<token>.json`）+ 重启恢复 + 过期同步删文件。
- **消费语义**：Consume 原子（锁内过期检查 `!now.Before(ExpiresAt)` 防 Windows 时钟 tick 误判 + MaxDownloads 计数 + OneTime 删除）；过期/超次数删除后返回 nil。
- **多租户**：List 只返回自己（空 owner admin 全量）；Revoke 跨租户视为不存在（防枚举）；createShare 符号链接拒绝 + 可读校验 + TTL 上限（maxShareTTL）+ 跨卷定位。
- **访问面**：Peek 先校验存在（tenantID 找根 + rel 仍属 user 桶纵深防御 + 跨卷重新定位）→ Consume → 流式传输（Cache-Control no-store + Referrer-Policy no-referrer）。
- **事件**：分享创建发布 EventShare（载荷不含 token/password）。

### 归档（`pkg/server/archive.go`）
- **流式打包**：io.Pipe + tar + gzip；客户端断开检查（goroutine 泄漏防护）。
- **安全**：validateArchiveFiles（ValidateFilePath + HasServiceInternalPrefix 拦截 .__ 内部目录）→ archiveFileRootFor（视图定位 fail-closed）→ addFileToTarDepth（Lstat 符号链接拒绝 + os.SameFile TOCTOU 交叉验证 + 100MB 单文件上限 + 深度 100）。
- **批量**：cloudArchiveTask（云归档任务 + max bytes 对账）。
- **测试**：share_persist_test/share_ro_test/share_test/version_crossvolume_test/version_delete_toctou_test/version_gc_loop_test 等；`go test` 全绿。

## 验证方式

- 源码逐路径审查（版本三操作 + 分享全生命周期 + 归档安全链）
- `go test -count=1 -timeout 180s -run 'TestShare|TestVersion|TestArchive' ./pkg/server/...` → **ok**
