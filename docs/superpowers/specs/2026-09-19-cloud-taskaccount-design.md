# cloud 任务配额所有权句柄收敛（TaskAccount）设计

日期：2026-09-19
状态：设计定稿（用户确认「① 所有权句柄收敛」根治方向 +「先规格文档再 TDD」+「原场景测试覆盖保证无回归」）
作者：pi agent（suixibing 需求，源自 CI run 35419872637 Vault job 的并发账本 flake）

## 1. 背景与问题

`TestCloudDownloadManager_ConcurrentResumeAndCancel`（quota_writer_test.go:972）在 CI（Vault job，-race）偶发失败：

```
并发后 Scope Usage()=90 > 磁盘占用=0（虚高/泄漏）
```

**根因（已系统化调试闭环）**：配额占用由**多个并行记账点 + 多处释放逻辑**管理，判据互相参照却不共享：

| 记账点 | 位置 | 语义 |
|---|---|---|
| `CloudTask.QuotaCommitted` | manager.go:59 | 任务在 Scope 的已确认占用（完成/失败后 = 实际大小） |
| `CloudTask.qw *quota.QuotaWriter` | manager.go（SinkFactory） | 下载中边写边记（reserved + written） |
| `quota.Scope.Usage` | pkg/quota | 桶级最终账本 |

**释放逻辑 7 处**：`releaseTaskScope` / `releaseAbandonedTaskScope` / `rollbackResumeLocked` / `cleanupExpiredOnce` / `failTask` / `CancelTask` / `DeleteTask`——各自用「任务状态 + running 标记 + 磁盘占用」的组合判据决定是否释放，**没有共享的「所有权」抽象**。

**本次泄漏窗口**（4×25 轮并发 resume+cancel）：
1. ResumeTask 置 `Status=pending` + `running=true` → 启动 goroutine（排队等信号量）；
2. 并发 CancelTask 置 `cancelled` + 删 .partial + `removeTaskDir`（Scope 释放因 running 推迟到 goroutine 退出）；
3. goroutine 拿信号量前又被**下一轮 ResumeTask 改回 pending** → 复查非 cancelled → 继续下载 → QW 边写边记 commit；
4. 最终 CancelTask 置 cancelled + 删文件 → goroutine 早退 → `releaseAbandonedTaskScope` 看 Status=**pending**（≠cancelled）→ **不释放** → QW committed（90 字节）**永久残留**。

**结构性脆弱**：每次并发交错暴露一个「释放点与所有权不匹配」的组合；修一个窗口，下一次新交错又暴露新组合（#290/#305 已各修一次）。

## 2. 目标（用户确认）

**所有权句柄收敛（TaskAccount）**：
1. 把「任务在 Scope 中的配额占用」收归**单一抽象** `quota.TaskAccount`，生命周期显式：创建立账 → 写盘 commitUp → 终态结算 → 删除释放；
2. `CloudTask.QuotaCommitted` + `CloudTask.qw` **合并为** `task.account *quota.TaskAccount`（单一事实源）；
3. **释放只有一条路径**（`account.Release()` 幂等），现有 7 处释放逻辑收敛为同一调用点；
4. 「是否还有 goroutine 在写」由 `m.running[taskID]` 唯一决定；account 释放与 running 清除**同锁内原子**完成 ⇒ 不变量「goroutine 已停 ⇒ 配额已归零」由**结构**保证，而非判据拼凑。

## 3. 设计

### 3.1 新抽象 `quota.TaskAccount`（pkg/quota 内）

```go
// TaskAccount 持有某任务在 Scope 中的配额占用（reserved + committed）的唯一所有权。
// 生命周期显式：NewTaskAccount（立账）→ CommitUp（写盘边写边记）→ Release（终态释放，幂等）。
// 内部自锁（与 QuotaWriter 同款叶锁），释放与写盘并发安全。
type TaskAccount struct {
    mu        sync.Mutex
    scope     *Scope
    reserved  int64
    committed int64
}

func NewTaskAccount(s *Scope, estimate int64) (*TaskAccount, error) // estimate<=0 → 1 GiB 占位
func (a *TaskAccount) CommitUp(n int64)                             // 写盘 n 字节：reserved→committed（不足先补留）
func (a *TaskAccount) ReleaseReserve()                              // 归还未用 reserve（保留 committed）
func (a *TaskAccount) Release()                                     // 释放全部（committed + reserved），幂等
func (a *TaskAccount) Committed() int64
func (a *TaskAccount) Reserved() int64
```

- **语义对齐现 QuotaWriter**：`CommitUp` 即现 `Write` 的记账部分（预留不足自动补留，补留失败返回错误）；
- `Release()` = 现 `releaseTaskScope` 的全部逻辑（ReleaseUsage(committed) + ReleaseReserve），**幂等**（committed/reserved 归零后空操作）；
- **不新建记账路径**：TaskAccount 是 QuotaWriter 的记账核心抽取，可继续由 QuotaWriter 包装成 downloader.QuotaSink（`quotaSinkAdapter` 保留，内部委托 TaskAccount）。

### 3.2 CloudTask 收敛

```go
type CloudTask struct {
    // 删除：QuotaCommitted int64 + qw *QuotaWriter
    account *quota.TaskAccount // 任务配额唯一所有权（json:"-"；快照置 nil）
}
```

- `downloadSinkFactory`：创建/复用 `task.account`（替代现 task.qw；重试/续传 SetWriter 语义保留在 adapter 内）；
- `releaseTaskScope(task)` → `task.account.Release()`（幂等，无占用/未装配空操作）；
- `releaseAbandonedTaskScope` → 判据简化为「`m.running[taskID]==false` 即释放」（account 释放与 running 清除同锁原子，见 3.3）；
- `rollbackResumeLocked` 的补充释放 → 同锁内 `account.Release()`；
- `cleanupExpiredOnce` → 锁内 `account.Release()` + 归零；
- `failTask` / `CancelTask` / `DeleteTask` → 锁内 `account.Release()`（替换各自散落的 ReleaseUsage/Adjust 对账）。

### 3.3 不变量（结构保证）

```
锁内原子：running 清除 ∧ account.Release()
```

- 所有释放点持 `m.mu` 写锁，且与 `delete(m.running, id)` 同临界区；
- `ResumeTask` 锁内复查：account 已释放（任务被取消/删除）→ 不重新立账（防「接管已释放 account」）；
- goroutine 退出路径（cleanupRunning defer）→ 同锁释放 + 清 running ⇒ `waitTaskStopped` 返回 true 即配额已归零。

### 3.4 行为等价（无回归承诺）

| 现行为 | 收敛后 | 等价性 |
|---|---|---|
| failTask 保留 .partial：QuotaCommitted 记录磁盘占用，不释放 | account.Committed() 保留，不 Release | 续传复用同一 account |
| force resume 删 .partial 后回拨（discardedSize） | account.Adjust/ReleaseReserve 语义保留 | 只回拨实际消失字节（#305 语义） |
| 完成：QuotaCommitted=result.Size | account.Committed() 收敛 result.Size | 删除/过期对账一致 |
| 取消：running 时推迟释放 | 同锁释放（goroutine 退出即释放） | 无「提前释放被后续 commit 抬回」 |
| 过期清理：QuotaCommitted ReleaseUsage | account.Release() | 幂等归零 |
| 周期 reconcile（pkg/server 两拍） | 不变（桶级账本外部对齐） | 不触碰 |

## 4. 原场景测试覆盖（无回归保证）

**全部既有配额/生命周期测试必须保持绿**（作为收敛的回归网）：
- `resume_tenant_test.go`（16 处账本断言）：租户不可用回滚 / 失败 .partial 保留 / 删除即释放 / 并发放弃（resumeWindowHook seam）→ Scope 归零
- `lifecycle_invariant_test.go`：lifecycleNoRunningMark / 窗口内并发放弃回滚补齐 / 不变量扫描
- `quota_writer_test.go`（13 处）：并发 resume+cancel / 存储满 / 配额归零 / 账本不反负不虚高
- `quota_sink_criteria_test.go`（5 处）：sink 装配判据 / 幻影账本防回归
- `cloud_toctou_test.go`（1 处）：完成/取消 TOCTOU 账本对账
- `manager_test.go`（1 处）：基础配额

**新增红灯测试**（钉住本次窗口，防止未来回归）：
- `TestTaskAccount_ConcurrentResumeCancel_NoLeak`（quota_writer_test.go 旁）：复现「cancel 后 resume 改回 pending 且 goroutine 退出」现场（用 resumeWindowHook seam 确定性注入）→ 断言 `scopeUsage==0`；
- `TestTaskAccount_Release_Idempotent`：account.Release() 多次调用/与写盘并发 → 幂等归零；
- `TestTaskAccount_Lifecycle`：创建→CommitUp→Release 全周期账本正确（含补留/占位）。

## 5. 实施边界（TDD 顺序）

1. **pkg/quota**：新增 `task_account.go`（TaskAccount）+ 单测（生命周期/幂等/并发）——纯新代码，无回归面；
2. **pkg/cloud**：`downloadSinkFactory` / `releaseTaskScope` 等 7 处收敛到 `task.account.Release()`——每处改完跑对应用例；
3. **红灯先行**：先加 `TestTaskAccount_ConcurrentResumeCancel_NoLeak`（用 resumeWindowHook）确认失败（当前泄漏）→ 收敛后转绿；
4. 全量门禁：`go test -race ./pkg/cloud/ ./pkg/quota/` + `make test/lint/lint-all/deadcode-check` + serial_budgets 登记（新增串行测试若适用）。

## 6. 不做（YAGNI）

- 不改 `pkg/server` 的周期 reconcile 两拍（桶级对齐属既有取舍，见 manager.go 注释）；
- 不合并 Scope/Pool 层（TaskAccount 只收敛任务级所有权，不动桶级账本结构）；
- 不做 account 持久化（QuotaCommitted 本就是 `json:"-"`，重启靠磁盘扫描校准——保留）。

## 7. 验证方式

- 红灯测试 `TestTaskAccount_ConcurrentResumeCancel_NoLeak` 在收敛前失败（复现泄漏）、收敛后通过；
- 全部既有配额/生命周期测试绿（§4 清单）——无回归；
- `go test -race ./pkg/cloud/ ./pkg/quota/` + `make test/lint/lint-all/deadcode-check` 全绿；
- CI 全绿（Vault job 含 -race 并发场景）。
