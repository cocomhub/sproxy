# 08 · 「快照浅拷贝」缺陷族：第三次出现 + 机械化（2026-11-27）

## 一、族谱

同一个缺陷出现了**三次**，每次都在不同入口：

| # | 入口 | 谁修的 | 表现 |
|---|---|---|---|
| 1 | `SnapshotTask` / `SnapshotTasks` | #1327 / #1329 | HTTP 解锁后序列化 vs 锁内原地写 |
| 2 | **`ListTasks`**（漏网） | 本会话 #1336 | 同上（Web UI 列表用的就是它） |
| 3 | **`findByURL`**（漏网） | 本会话本轮 | 同上，且快照被 `CreateTask` **直接返回给调用方** |

根因是同一个：`CloudTask` 含**被原地改写**的切片/指针 ——
`Chunks` 底层数组（`recordChunkState` 对同 index 是 `task.Chunks[i] = st`）、
`*Diagnostics`（`recordKeptPartial` 锁内写）、`Progress`、`MetaExtra` map。
所以「值拷贝 + 返回指针」= 共享可变状态，快照一旦**逃逸**到解锁后的步骤（序列化、日志、回显），
就与后续锁内写无同步。

## 二、为什么这类缺陷特别难发现

1. **没有内容断言能发现它**：返回的字段值都是对的，只是"以后会变"。
2. **`-race` 只在特定逃逸形态命中**：如果调用方"拿到就立刻序列化"，mutex 恰好提供了顺序
   （`RUnlock` 与下一次 `Lock` 之间有 happens-before）⇒ **不报**。必须显式建模
   「快照逃逸 + 延迟序列化」才复现（#1336 的用例就是这么写的）。
3. **每次修的都是"被报出来的那一个入口"**，同族其它入口不红。

## 三、本轮修第三处

`findByURL`（`pkg/cloud/manager_task.go:1687`）在 URL 去重命中时把任务返回给
`CreateTask`，而 `CreateTask` 的返回值**就是给 HTTP 调用方的**：

```go
c := *t
c.account = nil
return &c      // ← 共享 Chunks 底层数组与 *Diagnostics
```

修：`return deepCopyTask(t)`。

断言（`pkg/cloud/find_by_url_snapshot_test.go`），两条独立证据：
- **确定性**：去重命中的快照 `&snap.Chunks[0] != &live.Chunks[0]`（不依赖时序）；
- **`-race`**：快照逃逸 + 延迟 `json.Marshal` 与锁内原地改写并发。

## 四、机械化：R62 门禁

`internal/archcheck/cloud_task_snapshot_gate_test.go`：

> `pkg/cloud` 里凡返回值含 `*CloudTask` 的函数，函数体不得用 `x := *y` 浅拷贝返回；
> 必须经 `deepCopyTask`（`deepCopyTask`/`snapshotTask` 自身是唯一合法浅拷贝点）。

### 为什么判据锚在「返回值类型」而不是「扫所有 `c := *t`」

值拷贝本身合法：`cloneTransferSpec` 拷贝的是 `TransferSpec`（顶层三字段无指针）；
`deepCopyTask` 自己也从值拷贝起步。只有**返回 `*CloudTask`** 的函数才存在"共享可变状态给调用方"的风险。

### 双侧护栏（防空转）

| 护栏 | 下限 | 作用 |
|---|---|---|
| 扫描源码文件数 | ≥10 | 目录被搬走 ⇒ 红（不是静默 0 命中） |
| 判定「返回 *CloudTask」函数数 | ≥6 | 判据失效（AST 解析失败）⇒ 红 |
| 走 `deepCopyTask` 的点数 | ≥3 | 只扫不算 ⇒ 红 |

> 这是 R61 那次"门禁空转"教训的延续：**扫描型门禁必须断言"我真的校验了 N 个"**。

### 变异（2/2 命中）

| 变异 | 门禁输出 |
|---|---|
| `findByURL` 改回浅拷贝 | `pkg\cloud\manager_task.go:1687:1 findByURL` |
| `ListTasks` 改回浅拷贝 | `pkg\cloud\manager_query.go:63:1 ListTasks` |

## 五、同类是否还有（已扫，结论：无）

全仓扫「值拷贝 + 返回指针」形态共 14 处，逐一核对为**安全**：

| 位置 | 为什么安全 |
|---|---|
| `files/chunked_store.go copySession` | 显式复制 `ReceivedChunks`/`ChunkChecksums` 两个切片 |
| `share/store.go`、`share/state_store.go`（3 处） | `Link` 成员均为值类型/字符串，无切片/map |
| `syncmgr/manager.go:672`（快照） | 显式复制 `Include`/`Exclude`/`Results` + `copyCarriers` |
| `syncmgr/manager.go:537`（去重复用） | `SyncTask` 无被原地改写的引用字段（进度走值字段） |
| `secretdata/features.go:549` | 显式复制 `Chunks` |
| `issues/issues.go:255`、`webdav/remote.go:89`、`conflict_index.go:200`、`pikpak/account.go:354`、`upload_recovery_wiring.go:54`、`http_downloader.go:107`、`turnrest.go:226` | 成员均为值类型（`turnrest` 还刻意清掉 `User` 凭证） |

## 六、给后续维护者的判据

写快照/查询函数时先问一句：**返回的结构体里有没有"被别处原地改写"的引用字段**？

- 有 ⇒ 必须深拷贝（`deepCopyTask` 这类唯一实现）；
- 没有 ⇒ 值拷贝可接受（R62 不会拦，因为它锚在类型上）。
