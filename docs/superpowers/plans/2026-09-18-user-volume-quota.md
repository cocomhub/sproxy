# P5：用户卷 quota per-owner 融合 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把用户卷/外部卷（baidupcs/WebDAV/S3）的 staging 配额从「卷级无限额记账」升级为「**per-owner 配额**」——staging 写入按任务 owner 分桶（owner_quotas 生效），防单用户占满本地磁盘。

**背景：**
- 前置：V3 + 三个后端已合并（master `5dc3b340`）：RegisterBackend 插件 + kind=volume + 用户卷 + owner_quotas 体系
- 现状（T4 遗留 Minor）：`scopeQuotaTracker` 用**卷级 Scope**（`NewPool(0).Scope("",0)`）——无限额只记账；工厂签名 `(ctx, remote)` **无 owner 上下文**（P5 要解）
- 用户确认（2026-09-18）：做 quota per-owner（P5）

**架构（owner 传递链）：**
```
syncmgr task.Owner（已有）
  → syncexec.Executor.Run(task, remote)  // task.Owner 可用
  → newRemoteFS(ctx, remote, task.Owner)  // 新增 owner 参数
  → BaidupcsFSFactory(ctx, remote, owner) // 工厂签名扩展
  → 装配层闭包：scopeFor(owner)（owner_quotas 的 user 桶 Scope）
  → StorageFS.WithQuota(&ownerQuotaTracker{scope: scopeFor(owner)})  // 按 owner 分桶
```

**关键设计（控制者）：**
- `BaidupcsFSFactory` 签名：`func(ctx context.Context, remote syncmgr.RemoteConfig, owner string) (syncpkg.FS, func(), error)`——新增 owner 参数（Run 传 task.Owner）。
- `newRemoteFS` 加 owner 参数（Run 调用处传 `task.Owner`）。
- `MeshFSFactory` **不动**（mesh 载体无本地 staging，owner 不相关）。
- 装配层（cmd/sproxy/baidupcs_sync.go / webdav / s3 backend 构造闭包）：
  - `setupBaidupcsFSFactory(exec, set, log, scopeFor func(owner string) *quota.Scope)`——scopeFor 由装配层注入（`h.SyncQuotaScope()`）
  - 工厂闭包内：`ownerScope := scopeFor(owner)` → `fs.WithQuota(&ownerQuotaTracker{scope: ownerScope})`（ownerScope nil → 不装配 quota，兼容无配额）
- `ownerQuotaTracker`（QuotaTracker 实现，仿 scopeQuotaTracker 但 scope 按 owner）：
  - `ReserveUsage(size)` → `scope.TryReserve(size)` + `Commit(size)`（计数器语义，Ruling-5 修正版）
  - `ReleaseUsage(size)` → `scope.ReleaseUsage(size)`
- 注意：StorageFS 是**每任务构造**（工厂每次调用新建）——owner 绑定在 FS 实例上，无跨任务串桶（并发安全）。

**影响面：**
- `pkg/syncexec/executor.go`（工厂签名 + newRemoteFS owner 参数）
- `cmd/sproxy/baidupcs_sync.go`（scopeQuotaTracker → ownerQuotaTracker + scopeFor 注入）
- `cmd/sproxy/root.go`（装配传 scopeFor）
- WebDAV/S3 backend：与 baidupcs 同构（若它们也有 staging quota——当前无 staging 直接写，可仅 baidupcs 做或统一接口）

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **行尾纪律**：改动后核查 `git ls-files --eol`（i/lf w/lf）。

---

### 任务 1：工厂签名扩展 + owner 传递

**文件：**
- 修改：`pkg/syncexec/executor.go`（`BaidupcsFSFactory` 加 owner 参数 + `newRemoteFS` 加 owner + Run 调用处传 task.Owner）
- 测试：`pkg/syncexec/executor_baidupcs_fs_test.go` + `executor_remote_kinds_test.go`（工厂调用断言 owner 传递）

**目标：** owner 从任务传递到工厂（后续 quota 分桶）。

- [ ] **步骤 1：读 BaidupcsFSFactory 定义 + newRemoteFS + Run 调用链**

- [ ] **步骤 2：编写失败的测试**

```go
func TestExecutor_Run_BaidupcsFS_OwnerPassed(t *testing.T) { t.Parallel() /* 工厂收 owner == task.Owner */ }
```

- [ ] **步骤 3：实现**（签名扩展 + newRemoteFS owner 参数 + Run 传 task.Owner）→ 验证通过（既有测试零回归——mock 工厂签名同步）

- [ ] **步骤 4：Commit**

```bash
git add pkg/syncexec/executor.go pkg/syncexec/*_test.go
git commit -m "feat(syncexec): BaidupcsFS 工厂加 owner 参数（quota 分桶前置）" --no-verify
```

---

### 任务 2：装配层 quota per-owner（ownerQuotaTracker + scopeFor 注入）

**文件：**
- 修改：`cmd/sproxy/baidupcs_sync.go`（`scopeQuotaTracker` → `ownerQuotaTracker`（scope 按 owner）；`setupBaidupcsFSFactory` 加 scopeFor 参数 + 工厂闭包内按 owner 装配 quota）
- 修改：`cmd/sproxy/root.go`（传 `h.SyncQuotaScope()` 作 scopeFor）
- 测试：`cmd/sproxy/baidupcs_sync_test.go`（owner 分桶断言：ownerA/ownerB 各自 Scope 计数独立）

**目标：** staging quota 按 owner 分桶（owner_quotas 生效）。

- [ ] **步骤 1：读 scopeQuotaTracker + setupBaidupcsFSFactory + h.SyncQuotaScope**

- [ ] **步骤 2：编写失败的测试**

```go
func TestOwnerQuotaTracker_ReserveRelease(t *testing.T) { t.Parallel() /* owner Scope 计数 */ }
func TestSetupBaidupcsFactory_OwnerScope(t *testing.T)  { t.Parallel() /* 工厂闭包按 owner 装配 quota */ }
```

- [ ] **步骤 3：实现**（ownerQuotaTracker + scopeFor 注入 + 工厂闭包按 owner WithQuota）→ 验证通过（ownerScope nil → 不装配，兼容）

- [ ] **步骤 4：Commit**

```bash
git add cmd/sproxy/baidupcs_sync.go cmd/sproxy/root.go cmd/sproxy/baidupcs_sync_test.go
git commit -m "feat(server): staging quota per-owner（ownerQuotaTracker + scopeFor 注入）" --no-verify
```

---

### 任务 3：e2e + 文档

**文件：**
- 修改：`cmd/sproxy/baidupcs_sync_e2e_test.go`（e2e 装配传 scopeFor + owner 分桶验证）
- 修改：`docs/`（quota per-owner 说明）

**目标：** 全链路验证（任务 owner → quota 分桶）+ 文档。

- [ ] **步骤 1：e2e**（任务 ownerA/ownerB 各自 staging 配额独立计数——owner_quotas 生效）

- [ ] **步骤 2：变异验证**（ownerScope nil → 不装配 quota；owner 传递去掉 → 测试红）

- [ ] **步骤 3：文档**（quota per-owner 说明：owner_quotas 对用户卷/外部卷 staging 生效）

- [ ] **步骤 4：Commit**

```bash
git add cmd/sproxy/baidupcs_sync_e2e_test.go docs/
git commit -m "test(baidupcs): quota per-owner e2e + 文档" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues（**sub-modules lint 本地必跑**）
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：owner 传递去掉 → 测试红；ownerScope nil 分支去掉 → 测试红
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
