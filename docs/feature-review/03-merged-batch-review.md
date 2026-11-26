# 03 · 他人近期合并代码对抗审查（2026-11-27）

**基线**：`4ab131be8` → `be8e6a17a`（含 #1325/#1327/#1329/#1330/#1331 等）
**结论**：发现 **1 项 P1**（`ListTasks` 浅拷贝快照，同族修复的漏网入口），已修 + 双重断言
（`-race` 复现 + 共享底层数组的确定性检查）；其余判为通过。

## 审查对象与结论

| PR | 能力 | 结论 |
| --- | --- | --- |
| #1327 / #1329 | 任务快照深拷贝（`deepCopyTask`）+ 提交响应返回快照 | ⚠️ **漏了 `ListTasks`** → 见 F1 |
| #1331 | 组快照深拷贝 `TaskIDs`（`deepCopyGroup`） | ✅ 通过（`GetGroup`/`ListGroups` 两个出口都换了；`TaskIDs` 是唯一共享切片） |
| #1330 | 本地卷 meta 记录客户端 mtime（先 `recordUploadSuccess` 再 sidecar） | ✅ 通过，附 P3 观察 |
| #1325 | secretdata 基线可见 / 重载三态 / 窗口就绪判据 / 卡片并发重放 | ✅ 通过（窗口就绪判据已含"写完后才就绪"语义） |

## F1（P1）`ListTasks` 仍是浅拷贝 —— 而它是 **Web UI 列表** 的入口

`#1327/#1329` 把 `SnapshotTask` / `SnapshotTasks` 改成 `deepCopyTask`，但
`ListTasks`（`GET /api/cloud/tasks`，列表页与传输页都用它）保留：

```go
if match && ownerVisible(t.Owner, owner) {
    c := *t
    c.account = nil
    all = append(all, &c)      // ← 浅拷贝：共享 Chunks 底层数组与 *Diagnostics
}
```

共享的东西恰好是**会被原地改写**的：

| 字段 | 写方 | 是否原地 |
| --- | --- | --- |
| `Chunks` | `recordChunkState` | **是**（同 index 覆盖 `task.Chunks[i] = st`） |
| `Diagnostics` | `recordKeptPartial` | 是（`*TaskDiagnostics` 指针共享，锁内写 `PartialPath/PartialSize`） |

HTTP 层在 `m.mu.RUnlock()` **之后**才 `json.Marshal`，快照一旦逃逸到后续步骤（另一个
goroutine / 后续循环），就与后续锁内写无同步。

**为什么普通`-race`用例抓不到**：若"拿完立刻序列化"，mutex 恰好提供了顺序
（`RUnlock` 与下一次 `Lock` 之间有 happens-before）⇒ 实测**不报**。
只有**快照逃逸后延迟序列化**才复现 ⇒ 测试必须显式建模这条路径。

- 修：`ListTasks` 改用 `deepCopyTask(t)`（与另两个出口同源）。
- 断言 1（确定性，不依赖时序）：快照的 `Chunks` **不得**与活动任务共享底层数组
  （`&snap.Chunks[0] == &live.Chunks[0]` ⇒ red）。变异回浅拷贝即红。
- 断言 2（`-race`）：快照逃逸 + 延迟 `json.Marshal` 期间写面继续持锁原地改写 ⇒ 修复前
  `WARNING: DATA RACE`，修复后绿。

## P3 观察（不改，记录理由）

### `#1330` 的事件早于 sidecar 可见

新顺序是「`recordUploadSuccess`（台账 + `Chtimes` + upload 事件）→ `writeMetaSidecar`」。
顺序调整本身正确（sidecar 的 mtime 取自落盘文件当前时间）。

副作用：订阅 `upload` 事件的消费者（SSE → v2 列表自动刷新 → 打开检查器）可能在 sidecar
写入前读 meta ⇒ 短暂"无 meta"。影响面 = 检查器的 meta 页签可能空一次（再刷新即好），
且 meta 失败本就走 `Warn` 兜底（读侧不校验）。属可接受的最终一致，不改。
若将来 meta 变成强依赖（如签名校验），应改为「先写 sidecar 再发事件」。

### `#1331` `deepCopyGroup` 的边界

`deepCopyGroup` 只深拷贝 `TaskIDs`；`CloudTaskGroup` 其余字段均为值类型
（`Name/Status/CreatedAt/...`），无其它共享引用 ⇒ 充分。

## 聚焦测试

`go test ./pkg/cloud -race -count=1`（49s 全绿）、`go build ./...`、
`go test ./pkg/server ./internal/archcheck`、`golangci-lint` 0 issues。
