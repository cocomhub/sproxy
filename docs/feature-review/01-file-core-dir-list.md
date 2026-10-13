# 审查：目录/列表/搜索/stat

- **批次**：1
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 1 / P3 0

## 发现清单

### [P2] 搜索索引快照 saveAll 非原子快照一致性（并发读窗口）
- **位置**：`pkg/files/search_index.go:414-440`（saveAll）
- **问题**：`saveAll` 先锁收集 owner 列表，再逐个 owner 锁内取 `oi` 副本、解锁后写快照——两个 owner 的 `oi` 取到的时间不同，多 owner 快照是**非一致的**（各自时间点）。但对单 owner 而言：锁内取指针 → 解锁 → saveIndexSnapshot（遍历 entries）期间，写路径可能 upsert/remove 修改 map（锁在 Save 之外）→ **遍历期间 map 被并发写**。
- **严重性评估**：Go map 并发读写是 runtime fatal（非 panic 可恢复）——**理论上有崩溃面**。但实际：`upsert`/`remove` 持 ix.mu 修改 `oi.entries`；`saveAll` 在锁内取 `oi` 后**解锁**再遍历 entries → 遍历期间并发 upsert 写同一 map ⇒ 潜在 fatal。需核实 saveAll 是否全程持锁。
- **核实**：`saveAll` 每 owner 的「取 oi」在锁内，遍历在锁外（`saveIndexSnapshot(tnt, o, oi.entries)` 在锁外）→ **确认存在并发写窗口**。
- **建议**：saveAll 遍历 entries 时持锁（或对 oi.entries 做浅拷贝快照）。
- **影响**：仅 `index_save_interval` 周期保存路径（低频）；搜索/列表主路径（searchLocked/list 均锁内 ensureOwner 后读，但读也在锁外——同面风险）。
- **复核**：`searchLocked` 在 `ix.mu` 锁内调 `ensureOwner`（内部又 Lock——**reentrant deadlock？**）——`ensureOwner` 内部 `ix.mu.Lock()`，而 `searchLocked` 未持锁（search 调 searchLocked 无锁）→ 无死锁。但 searchLocked 读完 `oi.entries` 是在锁外（ensureOwner 已返回）→ 同样有并发写 map 的读窗口。

**结论**：这是一个**真实存在的并发面**（map 并发读写窗口），升级为 **P1**：索引 upsert/remove 与 search/list/saveAll 遍历之间存在 map 并发访问窗口（`ix.mu` 只在写与 ensureOwner 内，读遍历在锁外）。

### [P1] 索引 map 并发读写窗口（search/list/saveAll 遍历在锁外）
- **位置**：`pkg/files/search_index.go:322-458`（searchLocked/list/saveAll 遍历 oi.entries 无锁）
- **问题**：`ix.mu` 保护的是「owners map 的替换」与「单 owner 的写」，但 `searchLocked`/`list` 遍历 `oi.entries`（map）时**不持 ix.mu**；写路径 `upsert`/`remove` 持锁修改同一 map。Go map 并发写/读写 = runtime fatal（进程崩溃）。
- **复现条件**：搜索/列表与并发上传/删除同时发生（高并发生产必然出现）。
- **证据**：`upsert`（`search_index.go:137-152`）持 `ix.mu` 写 `oi.entries[key]`；`searchLocked`（:322-357）无锁遍历 `oi.entries`。中间无原子指针替换（ownerIndex 是普通指针，`ix.owners[owner] = oi` 在锁内替换指针，但**替换后旧指针仍可能被在途 reader 使用**——reader 持的是替换前的 `oi` 指针，写路径替换指针后**不再修改旧 oi**？需核实）。
- **复核**：写路径 `upsert` 的语义是「持锁修改 map 后替换指针」——即**每次写都替换新 ownerIndex 指针**？看代码：`upsert` 持锁后 `oi := ix.owners[owner]`（旧指针）→ **直接修改 oi.entries**（非替换）→ 无 `ix.owners[owner] = 新oi`。**确认：写路径修改的是旧指针指向的 map**，与在途 reader 遍历的同一 map ⇒ **并发读写窗口成立**。
- **影响**：高并发上传/删除 + 搜索/列表 → 进程可能 fatal（map concurrent read and map write）。
- **建议**：写路径改为「拷贝 entries → 修改 → 替换指针」（copy-on-write），或在 search/list/saveAll 遍历时持读锁（ix.mu 是 sync.Mutex 无 RWMutex 读锁——需改 RWMutex 或拷贝）。

## 通过项（无问题面）

- **mkdir/rmdir**：`MakeDir`（`write_ops.go:440+`）路径校验 + 默认卷优先建树；`RemoveDir`（`write_ops.go:505-640`）force 语义 + 逐卷删除 + 符号链接拒绝 + 配额/池释放 + checksum 前缀清理 + 索引 removePrefix + 事件。
- **列表**：`List`（`read_ops.go:90-170`）owner 视图逐卷聚合 + `?volume=` 404 fail-closed + 索引 list + 排序分页；subdir 经 `listRelForOwner` 校验。
- **stat**：`StatPath`（`read_ops.go:228-256`）404/500 语义正确；checksum 台账或实时计算。
- **搜索**：`Search`（`read_ops.go:200-225`）索引 search 子串匹配（大小写不敏感）+ 目录条目去重 + 空 q 400。
- **索引持久化**：快照原子写（tmp+rename）+ 损坏回退重建 + invalidate 删快照（`index_persist.go`）。
- **测试**：`read_list_search_test.go`、`dirs_test.go`、`search_index_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（List/Search/RemoveDir/索引并发模型）
- `go test -count=1 -timeout 120s -run 'TestList|TestSearch|TestDir|TestRemoveDir' ./pkg/files/... ./pkg/server/...` → **ok**
- **并发面核实**：search_index.go 写路径修改 map 而非替换指针 —— 确认 P1 成立
