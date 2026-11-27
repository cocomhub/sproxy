# 09 · R62 泛化：同族缺陷在 `pkg/syncmgr` 再犯（2026-11-27）

## 一、情况

R62（上一 PR）只覆盖 `pkg/cloud` 的 `*CloudTask`。按"同一模式换包会不会再犯"扫 `pkg/syncmgr`，
**立刻找到同族缺陷**：

```go
// Manager.CreateTask 去重命中（manager.go:537）
c := *t
m.mu.Unlock()
return &c, false, nil     // ← 直接被 HTTP 层拿去序列化
```

`SyncTask` 的引用字段：

| 字段 | 被谁原地改写 |
|---|---|
| `Results` | `backfillRetryResults`：写锁内 `stored.Results[i].Action/Error/Checksum = ...` |
| `Carriers` | 执行结束回填（`task.Carriers = ...` 整体替换，但 map 仍与快照共享） |
| `Include` / `Exclude` | 创建期写入后只读（仍属引用类型） |

⇒ 浅拷贝返回后，调用方读到的 `Results` 与写锁内改写**共享底层数组**。

## 二、修复

抽**唯一**深拷贝实现 `deepCopySyncTask`，`CreateTask` 去重复用 与 `Get` 都改走它
（`Get` 原本就手工复制了四个字段 —— 说明当时已意识到问题，但去重出口漏了）。

顺带修两个「返回活动对象」的出口：

- `FanoutChildren`（返回 `[]*SyncTask`）：HTTP 扇出聚合视图会序列化它们；
- `GetFanoutChild`（返回 `*SyncTask`）。

再顺带修 `ConflictIndex.Get`（`pkg/syncmgr/conflict_index.go:193`）：`ConflictItem` 含
`Ours`/`Theirs` 两个 `[]string`。当前只在构造期写（未复现竞态），但代价极低 ⇒ 加
`deepCopyConflictItem`。这一类属"未爆但同形"，值得记：**判据是结构形态，不是"是否已经爆过"**。

## 三、R62-b 门禁（泛化版）

`internal/archcheck/task_snapshot_shared_gate_test.go`：

> `pkg/cloud` + `pkg/syncmgr` 里，返回值含指针型结构的函数，函数体不得「只值拷贝 `x := *y` 后
> `return &x`」——必须调 `deepCopy*`/`snapshot*`/`clone*` 助手，或有**字段级**容器复制。

豁免：构造函数（`New*`/`new*`，返回新对象而非既有对象快照）与深拷贝助手自身。

### 关键教训：判据必须精确到字段级

首版把**任意** `append`/`copy` 调用当成"深拷贝信号" ⇒ 大函数里随便一个 `append(xs, y)` 就放行：

```
# 变异：CreateTask 改回浅拷贝
$ go test ./internal/archcheck -run TestR62b
ok   ← 漏报！变异不红
```

改成「**LHS 是 `X.Field`，RHS 是容器复制调用**」后才命中：

```
$ go test ./internal/archcheck -run TestR62b
pkg/syncmgr/manager.go:519:1 CreateTask     ← 变异命中
```

> **门禁的判据粒度决定它有没有用**。粗判据的"绿灯"比没有门禁更危险（给人已覆盖的错觉）。
> 每次加扫描型门禁，都必须先跑一遍变异确认红 —— 这已经是本仓第三次靠变异发现门禁自身失效
> （R61 空转、R62 假绿豁免、R62-b 粗判据）。

## 四、变异（2/2 命中，含"粗判据漏报"的修复过程）

| 变异 | 结果 |
|---|---|
| `CreateTask` 去重改回 `c := *t` | ✅ 点名 `pkg/syncmgr/manager.go:519:1 CreateTask` |
| `ConflictIndex.Get` 改回 `c := *it` | ✅ 点名 `pkg/syncmgr/conflict_index.go:193:1 Get` |
| （首版判据）同上两例 | ❌ **漏报**（粗判据放行）→ 已改字段级 |

## 五、`pkg/cloud -race` 全包失败与本次无关（记录取证）

本 PR **未改动 `pkg/cloud`**（`git diff --name-only origin/master | grep cloud` 为空）。
全包 `-race` 偶发多例超时（`manager_test.go:568 timeout waiting for async download`、
`Close() hung: download goroutine leaked` 等）。取证：

- 单独跑失败用例 ⇒ **全绿**（`TestCloudDownloadManager_RecoverRestartsDownloading` 4.1s OK）；
- `-parallel 4` 跑全包 ⇒ **全绿**（94s）。

⇒ 属「全包并行负载下的超时」（20 核机器上 `-race` + 全包 `t.Parallel` 抢 CPU，30s 等待不够），
非逻辑缺陷。已记入 scratchpad 作为候选 flake，不在本 PR 处理（避免夹带无关改动）。

## 六、给后续维护者的判据（更新版）

写任何「把内部对象交出去」的函数前，三问：

1. 返回的结构体里有**切片 / map / 指针**字段吗？
2. 这些字段会被**别人原地改写**吗（不只是整体替换）？
3. 快照会**逃逸**到锁外/后续步骤吗？

只要 1 是"有"，就按 2/3 判断：**有原地改写 + 会逃逸 ⇒ 必须深拷贝**。
判据是形态（1），不是"是否已经爆过"（2/3）—— `ConflictItem` 就是形态命中但尚未爆的例子。
