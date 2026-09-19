# cloud 任务配额所有权句柄收敛（TaskAccount）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 cloud 下载任务的配额占用收归单一抽象 `quota.TaskAccount`（reserved+committed 唯一所有权），现有 7 处释放逻辑收敛为 `account.Release()` 一个幂等调用点，根治「并发 resume+cancel 后 Scope Usage 虚高」的账本泄漏。

**架构：** pkg/quota 新增 `TaskAccount`（QuotaWriter 记账核心抽取：创建立账 → CommitUp 写盘边写边记 → Release 幂等释放）；pkg/cloud 把 `CloudTask.QuotaCommitted`+`task.qw` 合并为 `task.account`，releaseTaskScope / releaseAbandonedTaskScope / rollbackResumeLocked / cleanupExpiredOnce / failTask / CancelTask / DeleteTask 全部收敛为同锁内 `account.Release()`；「running 清除 ∧ Release」同临界区 ⇒ 不变量「goroutine 已停 ⇒ 配额已归零」由结构保证。

**技术栈：** Go 1.27，pkg/quota（Scope/Pool/QuotaWriter），pkg/cloud（CloudDownloadManager），标准库 sync.Mutex。

**规格：** `docs/superpowers/specs/2026-09-19-cloud-taskaccount-design.md`（本计划的论证依据；执行者两份都读）

## 全局约束

- `quota.TaskAccount` 字段：`mu sync.Mutex` / `scope *Scope` / `reserved int64` / `committed int64`；方法 `NewTaskAccount(s *Scope, estimate int64) (*TaskAccount, error)`（estimate<=0 → 1 GiB 占位）、`CommitUp(n int64) error`（reserved→committed，不足先补留，补留失败返回 ErrStorageFull 且不改状态）、`ReleaseReserve()`（归还未用 reserve 保留 committed，幂等）、`Release()`（ReleaseUsage(committed) + ReleaseReserve，幂等归零）、`Committed() int64`。
- `CloudTask`：删除 `QuotaCommitted int64` 与 `qw *QuotaWriter` 字段，新增 `account *quota.TaskAccount`（`json:"-"`；快照拷贝端置 nil）。
- 释放点收敛：`releaseTaskScope` / `releaseAbandonedTaskScope` / `rollbackResumeLocked` 补充释放 / `cleanupExpiredOnce` / `failTask` / `CancelTask` / `DeleteTask` 全部改为同锁内 `task.account.Release()`；`releaseAbandonedTaskScope` 判据简化为「`m.running[taskID]==false` 即释放」（调用方须持 m.mu 且与 `delete(m.running, id)` 同临界区）。
- `ResumeTask` 锁内复查：`task.account == nil`（任务被取消/删除已释放）→ 不重新立账（防接管已释放 account）；`downloadSinkFactory` 创建/复用 `task.account`（替代 task.qw；SetWriter 语义保留在 quotaSinkAdapter 内委托 account）。
- **行为等价**（规格 §3.4）：failTask 保留 .partial（account.Committed 保留不 Release）；force resume 删 .partial 后只回拨实际消失字节（#305 语义）；完成时 account.Committed 收敛 result.Size；取消 running 时释放推迟到 goroutine 退出（同锁）；周期 reconcile（pkg/server 两拍）不触碰。
- 全仓硬规则：Go 源文件 SPDX 头；测试纯标准库；顶层 `func TestX` 默认 `t.Parallel()`（无法并发须在 `internal/archcheck/serial_budgets.tsv` 登记理由 + 函数体 `// sproxy:serial:` 注释）；提交 Conventional Commits 无署名行。
- **回归网**：规格 §4 列出的既有测试（resume_tenant_test / lifecycle_invariant_test / quota_writer_test / quota_sink_criteria_test / cloud_toctou_test / manager_test）必须全部保持绿。
- 运行测试：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 -race ./pkg/cloud/ ./pkg/quota/`；门禁 `make test/lint/lint-all/deadcode-check`。

---

### 任务 1：pkg/quota 新增 TaskAccount + 单测

**文件：**
- 创建：`pkg/quota/task_account.go`
- 测试：`pkg/quota/task_account_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
package quota

import (
	"testing"
)

// TestTaskAccount_Lifecycle 创建→CommitUp→Release 全周期账本正确（含补留/占位）。
func TestTaskAccount_Lifecycle(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)

	// 占位：estimate<=0 → 1 GiB。
	acc, err := NewTaskAccount(sc, 0)
	if err != nil {
		t.Fatalf("NewTaskAccount(0): %v", err)
	}
	if acc.Reserved() != placeholderReserve || acc.Committed() != 0 {
		t.Fatalf("占位后 reserved=%d committed=%d", acc.Reserved(), acc.Committed())
	}
	// CommitUp 划转 reserved→committed。
	if err := acc.CommitUp(10); err != nil {
		t.Fatalf("CommitUp(10): %v", err)
	}
	if acc.Committed() != 10 || acc.Reserved() != placeholderReserve-10 {
		t.Fatalf("CommitUp 后 committed=%d reserved=%d", acc.Committed(), acc.Reserved())
	}
	// Release 释放全部（committed + reserved）。
	acc.Release()
	if sc.Usage() != 0 || sc.Reserved() != 0 {
		t.Fatalf("Release 后 Scope usage=%d reserved=%d want 0", sc.Usage(), sc.Reserved())
	}
}

// TestTaskAccount_CommitUp_ReserveTopup 写超预留自动补留。
func TestTaskAccount_CommitUp_ReserveTopup(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)
	acc, err := NewTaskAccount(sc, 100)
	if err != nil {
		t.Fatalf("NewTaskAccount(100): %v", err)
	}
	// 补留 200 字节（超预留）。
	if err := acc.CommitUp(200); err != nil {
		t.Fatalf("CommitUp(200): %v", err)
	}
	if acc.Committed() != 200 || acc.Reserved() != 0 {
		t.Fatalf("补留后 committed=%d reserved=%d", acc.Committed(), acc.Reserved())
	}
	acc.Release()
}

// TestTaskAccount_Release_Idempotent Release 幂等（多次调用/与写盘并发归零）。
func TestTaskAccount_Release_Idempotent(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)
	acc, err := NewTaskAccount(sc, 50)
	if err != nil {
		t.Fatalf("NewTaskAccount: %v", err)
	}
	_ = acc.CommitUp(20)
	acc.Release()
	acc.Release() // 幂等
	acc.ReleaseReserve()
	if sc.Usage() != 0 || sc.Reserved() != 0 {
		t.Fatalf("幂等后 Scope usage=%d reserved=%d want 0", sc.Usage(), sc.Reserved())
	}
}

// TestTaskAccount_CommitUp_ErrStorageFull 补留失败不改状态。
func TestTaskAccount_CommitUp_ErrStorageFull(t *testing.T) {
	t.Parallel()
	pool := NewPool(100) // 上限 100 字节
	sc := pool.newScope("cloud", 100)
	acc, err := NewTaskAccount(sc, 50)
	if err != nil {
		t.Fatalf("NewTaskAccount: %v", err)
	}
	if err := acc.CommitUp(200); err == nil {
		t.Fatal("补留超限应返回 ErrStorageFull")
	}
	// 状态不变：仍 50 预留、0 committed。
	if acc.Reserved() != 50 || acc.Committed() != 0 {
		t.Fatalf("补留失败后 reserved=%d committed=%d want 50/0", acc.Reserved(), acc.Committed())
	}
	acc.Release()
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 ./pkg/quota/`
预期：FAIL（`TaskAccount` 未定义 / `NewPool(0).newScope` 签名不符——用既有测试同款构造）

- [ ] **步骤 3：实现 TaskAccount**

`pkg/quota/task_account.go`：`TaskAccount` 结构（mu/scope/reserved/committed）+ `NewTaskAccount`（预留 estimate 或 1 GiB 占位，复用 `pool.reserveUp`）+ `CommitUp(n)`（`reserveUp` 补留 → `commitUp(n, n)` → reserved-=n / committed+=n，与 QuotaWriter.Write 记账同构）+ `ReleaseReserve`（`releaseUp(reserved)` 归零，幂等）+ `Release`（`scope.ReleaseUsage(committed)` + `ReleaseReserve`，幂等归零）+ `Committed`/`Reserved` 访问器。SPDX 头。内部方法（reserveUp/commitUp/releaseUp）同包可直接调用（对照 quota_writer.go 既有用法）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 -race ./pkg/quota/`
预期：PASS（新增 4 用例 + 既有 quota 测试全绿）

- [ ] **步骤 5：Commit**

```bash
git add pkg/quota/task_account.go pkg/quota/task_account_test.go
git commit -m "feat(quota): TaskAccount 所有权句柄 — 立账/CommitUp/幂等 Release（生命周期单测）"
```

---

### 任务 2：CloudTask 收敛 + downloadSinkFactory 复用 account

**文件：**
- 修改：`pkg/cloud/manager.go`（CloudTask struct、downloadSinkFactory、quotaSinkAdapter、releaseTaskScope）
- 测试：`pkg/cloud/quota_sink_criteria_test.go`（既有，须保持绿）+ 新增 `pkg/cloud/task_account_test.go`（收敛后语义）

- [ ] **步骤 1：编写失败的测试（CloudTask 收敛后 sink 复用）**

```go
// 在 pkg/cloud/task_account_test.go（同包）：
// 收敛后：downloadSinkFactory 创建 task.account；releaseTaskScope 收敛为 account.Release。
func TestCloudTask_Account_ReuseAcrossRetries(t *testing.T) {
	t.Parallel()
	// 用 newCloudTestEnv + newCloudTestManager（manager_test_common_test.go 既有辅助）
	// 装配带配额 Scope 的管理器；CreateTask 后手动创建 task.account（模拟首轮会话），
	// 断言 downloadSinkFactory 复用同一 account（非新建）。
}
```

（此测试在实现前编译失败——`task.account` 字段不存在 → 红灯；实现后转绿）

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（`task.account` 未定义）

- [ ] **步骤 3：实现收敛**

manager.go：`CloudTask` 删除 `QuotaCommitted int64` 与 `qw *QuotaWriter`，新增 `account *quota.TaskAccount`（`json:"-"`）；`downloadSinkFactory` 的闭包内 `task.account` 创建/复用（替代 task.qw：`NewTaskAccount(scope, estimate)` 或 `account` 已有则复用）；`quotaSinkAdapter` 内部委托 account（`Write` → `account.CommitUp` + 底层 writer；`Finish(success, oldSize)` → 成功 `account.ReleaseReserve` + `scope.ReleaseUsage(oldSize)`、失败 `account.ReleaseReserve`；`Committed()` → `account.Committed()`）；`releaseTaskScope(task)` → `if task.account != nil { task.account.Release(); task.account = nil }`（幂等）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 -race ./pkg/cloud/`
预期：PASS（quota_sink_criteria_test 等既有用例绿；若失败逐个对齐收敛语义）

- [ ] **步骤 5：Commit**

```bash
git add pkg/cloud/manager.go pkg/cloud/task_account_test.go
git commit -m "refactor(cloud): CloudTask 配额字段收敛为 task.account + sink 复用（TaskAccount 委托）"
```

---

### 任务 3：释放点收敛（7 处 → account.Release）

**文件：**
- 修改：`pkg/cloud/manager.go`（releaseAbandonedTaskScope 判据简化）
- 修改：`pkg/cloud/manager_lifecycle.go`（rollbackResumeLocked 补充释放、ResumeTask 锁内复查）
- 修改：`pkg/cloud/manager_persist.go`（cleanupExpiredOnce）
- 修改：`pkg/cloud/manager_task.go`（failTask、下载完成路径）
- 修改：`pkg/cloud/manager_query.go`（CancelTask、DeleteTask）
- 测试：`pkg/cloud/resume_tenant_test.go` / `lifecycle_invariant_test.go` / `quota_writer_test.go` / `cloud_toctou_test.go`（既有，全部保持绿）

- [ ] **步骤 1：编写红灯测试（本次泄漏窗口，resumeWindowHook 确定性复现）**

在 `pkg/cloud/quota_writer_test.go` 末尾追加（或新建 `task_account_leak_test.go`）：

```go
// TestCloudTask_ConcurrentResumeCancel_NoLeak 钉住「cancel 后 resume 改回 pending 且
// goroutine 退出」的账本泄漏窗口（CI run 35419872637 Vault job 现场）。
// 用 resumeWindowHook seam 在 ResumeTask 的 unlock→relock 窗口注入 CancelTask，
// 确定性构造「resume 置 pending/running 后立即被取消」→ 断言 Scope 归零。
func TestCloudTask_ConcurrentResumeCancel_NoLeak(t *testing.T) {
	t.Parallel()
	re := newResumeTenantEnv(t)
	mgr, owner := re.mgr, "alice"

	task, err := mgr.CreateTask("url", "https://example.com/leak.bin", "leak.bin", 100, owner)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// 首次下载失败保留 .partial（QW committed=90）→ failed。
	taskDir := filepath.Join(mgr.CloudDirFor(owner), task.ID)
	if err = os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err = os.WriteFile(filepath.Join(taskDir, "leak.bin.partial"), make([]byte, 90), 0o644); err != nil {
		t.Fatalf("写 .partial: %v", err)
	}
	mgr.failTask(task, "simulated failure")
	if got := re.scopeUsage(owner); got != 90 {
		t.Fatalf("前置不成立：failTask 后 Scope=%d want 90", got)
	}

	// resumeWindowHook：在 ResumeTask 的 unlock→relock 窗口里 CancelTask（置 cancelled +
	// 删文件 + 释放存储；Scope 因 running 推迟到 goroutine 退出）。
	origHook := resumeWindowHook
	resumeWindowHook = func(m *CloudDownloadManager, taskID string) {
		if taskID == task.ID {
			_ = m.CancelTask(taskID, owner)
		}
	}
	t.Cleanup(func() { resumeWindowHook = origHook })

	if err := mgr.ResumeTask(task.ID, false, owner); err == nil {
		t.Fatal("窗口内 cancel 后 ResumeTask 应失败（任务已取消）")
	}
	// 终态断言：Scope 必须归零（releaseAbandonedTaskScope 判据漏 pending 时残留 90）。
	if got := re.scopeUsage(owner); got != 0 {
		t.Fatalf("取消后 Scope=%d want 0（泄漏窗口：resume 改回 pending 且 goroutine 已退）", got)
	}
	if got := re.cloudUsage(); got != 0 {
		t.Fatalf("取消后全局账本=%d want 0", got)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 -run 'TestCloudTask_ConcurrentResumeCancel_NoLeak' ./pkg/cloud/`
预期：FAIL（`scopeUsage=90` 残留——泄漏窗口真实存在，红灯钉住）

- [ ] **步骤 3：实现释放点收敛**

逐处替换（每处改完跑对应测试）：
- `releaseAbandonedTaskScope`（manager.go:543）：判据从「`ok && Status != "cancelled"` 不释放」简化为「`m.running[task.ID]` 为真不释放，否则 `account.Release()`」——调用方（executeDownload 的 cleanupRunning defer）已持 m.mu 且同临界区清 running ⇒ pending 覆盖 cancelled 时 goroutine 已退 → running 已清 → 释放；
- `rollbackResumeLocked`（manager_lifecycle.go:54）：补充释放改为同锁 `account.Release()`（判据「任务不在 m.tasks 或 cancelled」保留，但释放统一走 account）；
- `ResumeTask` 锁内复查：`task.account == nil` → 不重新立账（已被取消/删除释放）；
- `cleanupExpiredOnce`（manager_persist.go:389）：`scopeCommitted` 收集改从 `task.account.Committed()`，释放 `account.Release()` + `task.account=nil`；
- `failTask`（manager_task.go:668）：qw 结算分支改为 account（`QuotaCommitted += qw.Committed()` → `account` 保留 committed 不 Release；storage-full 分支 `releaseTaskScope` → `account.Release()`）；
- 下载完成路径（manager_task.go 终态提交）：`stored.QuotaCommitted = result.Size` → `account` 收敛（不 Release，删除/过期对账时释放）；
- `CancelTask`（manager_query.go:101）/ `DeleteTask`：`releaseTaskScope` 调用自动收敛（内部已是 account.Release）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount && go test -count=1 -race ./pkg/cloud/ ./pkg/quota/`
预期：PASS（红灯测试转绿 + 全部既有配额/生命周期测试绿）

- [ ] **步骤 5：Commit**

```bash
git add pkg/cloud/manager.go pkg/cloud/manager_lifecycle.go pkg/cloud/manager_persist.go pkg/cloud/manager_task.go pkg/cloud/manager_query.go pkg/cloud/quota_writer_test.go
git commit -m "fix(cloud): 配额释放收敛为 task.account.Release — 根治并发 resume/cancel 账本泄漏
（releaseAbandonedTaskScope 判据简化 + ResumeTask 锁内复查 + 7 处释放点统一）"
```

---

### 任务 4：门禁 + 全量回归

**文件：**
- 修改：`internal/archcheck/serial_budgets.tsv`（如新增串行测试）
- 测试：全量

- [ ] **步骤 1：全量测试**

运行：
```bash
cd /d/workdir/leon/cocomhub/sproxy/.worktrees/cloud-taskaccount
export PATH="$PATH:$(go env GOPATH)/bin"
go test -count=1 -race ./pkg/... ./internal/archcheck/ 2>&1 | grep -E "^(--- FAIL|ok |FAIL)"
```
预期：全 ok（重点 pkg/cloud、pkg/quota；规格 §4 回归网清单逐项绿）

- [ ] **步骤 2：门禁**

运行：`make test && make lint && make lint-all && make deadcode-check && make web-test`
预期：全绿；serial_budgets 若超基线登记新文件（task_account_test.go / task_account_leak_test.go）。

- [ ] **步骤 3：原场景测试覆盖核验（无回归承诺）**

运行：`go test -count=1 -v -run 'TestCloudDownloadManager_ConcurrentResumeAndCancel|TestCloudDownloadManager_ResumeTaskTenantUnavailableRollsBack|TestCloudTask_ConcurrentResumeCancel_NoLeak|TestCloudDownloadManager_StorageFull|TestCloudTask_Account_ReuseAcrossRetries' ./pkg/cloud/`
预期：全部 PASS（并发场景 / 租户回滚 / 存储满 / 新泄漏窗口全绿）

- [ ] **步骤 4：Commit（如有登记）**

```bash
git add internal/archcheck/serial_budgets.tsv
git commit -m "chore(cloud): serial_budgets 登记 TaskAccount 新增测试文件（T4 门禁）"
```

- [ ] **步骤 5：收尾检查**

```bash
gofmt -l pkg/ && goimports -l pkg/
git status --porcelain   # 只含本任务文件
```
推送 `feat/cloud-taskaccount` → `gh pr create` → 等 CI 全绿（含 Vault job -race）→ `gh pr merge --squash --delete-branch`（commit_title 带 `(#PR号)`）→ `git pr-clean` + worktree remove + 主工作区同步。
