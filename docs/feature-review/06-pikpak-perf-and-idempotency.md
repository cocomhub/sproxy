# 06 · pikpak 性能修复审查 + 幂等重传验收 + 覆盖冲突决策（2026-11-27）

## 一、`#1342` + `#1343` 审查结论：通过（3 条 P3 观察，不改）

`#1342` 把 manifest 从「每次全量读盘 + 解析 + 全量写回」改成**内存镜像 + 节流写盘**；
`#1343` 进一步把子块进度收敛到 audit WAL（manifest 退化为整块检查点）。
两者都是 `O(n²) 读放大` 的正解（64 GiB / 1 MiB 子块 = 65536 次读改写）。

审查逐点核对（结论：无缺陷）：

| 检查点 | 结论 |
|---|---|
| 锁范围 | `manifestMu` 不再覆盖「读盘 + 解析」；`manifestLocked` 只在首次懒加载时读盘。`markSub` 只在锁内改内存位图，写盘在 `persistSubMask` 锁内（无 journal 时）或 WAL 追加（有 journal 时）——不会与 `markChunkDone` 交叉死锁（`mergeJournalMasks` 的加锁点没有外层持锁调用方） |
| 恢复语义 | WAL 的子块掩码按 **位 OR** 合并（`mergeJournalMasks`），且整块已完成时跳过位图 ⇒ 过期掩码不会吃掉新进度 |
| 进度虚增修复 | 尾块按 `dc.currentTotal - off` 钳制（`min(onesCount*1MiB, chunkLen)`），此前尾块不足 1 MiB 时进度会虚高 |
| 崩溃丢失上限 | 节流 8 子块 / 5s ⇒ 最坏丢 ≤8 MiB 子块进度（远优于整 chunk ≤64 MiB）；整块完成立即落盘 |

### P3 观察（不改，附理由）

1. **`markSub` 的掩码快照可能"后退"**：`persistSubMask(dc, c, mask, src)` 收到的是加锁前的
   快照，并发对**同一 chunk** 调用时后写者可能持久化较旧的掩码。当前每 chunk 由单 goroutine
   驱动（`downloadOneChunk` 串行跑 runs），且 WAL 恢复是 OR 合并 ⇒ 即便发生也只多下几个子块，
   不会丢数据。若要彻底消除，需在锁内重读位图再写盘（会重新引入一次读）。
2. **`markChunkDone` 清 `manifest.Blocks` 但不清 `dc.masks`**：看似会留一个"全满"位图导致
   该 chunk 再下载时 `subRuns` 为空、静默跳过。实际不可达——完成的 chunk 在 `manifest.Chunks`
   里，`dropZeroTailChunks` 剔除嫌疑块时 `Blocks` 已被清，续传路径从 `manifest.Blocks`
   重建 `dc.masks`。属"当前不可达"，但仍是脆弱耦合，值得后续加一条断言。
3. **测试计数器在生产结构体上**（`manifestWrites` / `journalWrites`）：可接受（零成本、
   便于断言写盘次数），但语义上是"测试观测字段"，放在同一结构体里需注释清楚。

## 二、nightly 新用例：`TestIdempotentReuploadRealVolumes`

锁「**同内容再传真的不重传**」，这是 `ChunkedUpload` 幂等短路的对外契约：

| 断言 | 意义 |
|---|---|
| 二次上传 `Success` | 不因幂等命中而报错 |
| `Stat` 的 size/checksum 不变 | 没有重写 |
| meta mtime = 客户端声明值 | 没有重写（重写会重新落 mtime） |
| **versioning 下版本数仍为 0** | **零重写的硬证据**：真覆盖会留下旧版本 |
| 下载内容完整 | 短路没有破坏文件 |

> 变异注意：该契约在客户端有**两层**短路（`tryResumeSession` 的 Completed 分支、init 的
> `already_exists`）——只改一层不会让 nightly 变红（实测：去掉 Completed 短路后，init 层
> 仍返回"上传已完成"）。因此本用例锁的是**行为**，单层契约由 `pkg/client` 的单测分别钉住。

## 三、决策：409 之后**不**自动「删旧 + 重传」

服务端对「同名不同内容」按策略拒绝：

| 形态 | 行为 |
|---|---|
| 显式指名卷 | **恒拒**（AD-4 卷唯一性查重） |
| 自动路由 + versioning 开 | 备份旧版本后覆盖 |
| 自动路由 + versioning 关 | 409 |

**决定**：客户端**不**自动删除旧文件后重传。理由：

1. 删除**不可逆**，而"覆盖"与"先删再传"在语义上不等价（删除会丢失旧内容与其侧写元数据）；
2. SDK 不应该替调用方猜意图——重传一个同名不同内容的文件，可能是"更新"也可能是"放错目录"；
3. 自动删除会让「一个配置错误」变成「批量删除用户文件」。

替代方案（已实现）：**把下一步写进错误**。
`初始化上传失败 (HTTP 409): …（目标路径已有不同内容的文件；请先删除/改名后重传，
或由服务端开启 versioning 以允许覆盖）`。用例 `TestChunkedUpload_ConflictErrorIsActionable`
钉住这段文案（去掉提示即红）。

若将来需要自动化，正确形态是**显式 opt-in**（如 `WithOverwriteTarget(true)` 或服务端
versioning），而不是默认删除。

---

## 七、追加审查 `#1347`（每片只追加 WAL）：一个持续性盲点（2026-11-27）

`#1347` 把「分片完成」从「整文件重写 manifest」改成「只追加一条 WAL 记录」，方向正确
（下载是连续操作，进程内只用内存；文件只在崩溃恢复时读）。

但它新增的兜底 **`manifestFallback` 是一次性的**：

```go
if jerr := dc.journal.ChunkDone(...); jerr != nil {
    d.log.Warn(...)        // 每块都 Warn
    d.manifestFallback(dc) // 每次都整文件 writeManifest
}
```

若 WAL **持续**失败（磁盘满、只读 FS、journal 被删），则**每个分片**都整文件重写 ⇒
**回到 #1342 要消除的 O(n²) 写放大**（64 GiB / 1 MiB 子块 ⇒ 65536 次全量重写），
且每块一条 Warn 会刷屏淹没真正的诊断信息。

### 修法

| 场景 | 行为 |
|---|---|
| 首次失败 | 立刻写一次**完整** manifest（把此前只在内存/WAL 的进度落盘，不留窗口）+ 置 `journalDegraded` |
| 持续失败 | 走 `persistDegraded`：与「无 WAL」路径**同一节流**（8 块 / 5s）⇒ 写放大回到 O(n) |
| 日志 | 只在**首次**降级时 Warn（持续故障不再刷屏） |

### 断言（`hybrid_journal_degraded_test.go`）

1. **降级标记置位**（可观测）；
2. **进度不丢**：内存镜像含全部 8 个分片，且至少落盘过一次（崩溃恢复依据存在）；
3. **写放大受控**：`manifestWrites < 分片数`（等于分片数就是 O(n²) 回归）。

第 2 条最初我写的是「最后一个分片必须已在**文件**里」—— **那条断言是错的**：节流的语义就是
「最新几块可能还没写」，文件里没有不等于丢进度（内存镜像才是进程内的单一事实源）。
被自己的用例打红后改正。

### 变异（命中）

把持续降级改回「每次都整文件写」：

```
hybrid_journal_degraded_test.go:67: 持续 WAL 故障下 manifest 全量重写 8 次（分片数 8）
⇒ 退化为 O(n²) 每块重写；应进入降级模式（回落一次后按节流写）
```

### 这条教训的推广

**「兜底路径」也要按稳态设计**。一次性兜底在**持续故障**下会退化成比原实现更差的形态
（这里是 O(n²) + 日志刷屏）。写任何 fallback 时问一句：**这个故障会持续吗？持续时会怎样？**
