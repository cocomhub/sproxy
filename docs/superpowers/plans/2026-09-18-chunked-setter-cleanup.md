# 分块上传：删除不门控导出 setter 清理计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。

**目标：** 删除三个**不门控**导出 setter（`SetSessionRoute` / `SetSessionStorageMgrReserved` / `SetSessionTempPath`）——生产零调用（仅测试用）、传 nil expect 不校验身份，是「同 id 接管」场景的不安全入口残留。用户确认：**允许破坏性变更，删除推荐**。

**背景（2026-09-18 控制者勘察）：**
- 两个数据面遗留（持久化乱序 + setter 缺身份校验）**已在 #313/#314 修复**（persistMu 会话内嵌锁 + gen 世代 + publishSessionIfCurrent 结构保证）
- 生产路径（chunked_upload.go）已全用 `setSessionRouteIfCurrent` / `setSessionStorageMgrReservedIfCurrent` / `setSessionTempPathIfCurrent` + `PersistNowIfCurrent`——**门控变体已覆盖全部生产发布**
- 遗留只剩：三个导出 setter（传 nil expect 不门控）生产零调用，仅测试用——删除消除不安全入口

**删除范围：**
- `pkg/files/chunked_store.go`：删除 `SetSessionRoute` / `SetSessionStorageMgrReserved` / `SetSessionTempPath`（保留 `setSession*IfCurrent` 门控变体）
- 测试文件调用点改用 IfCurrent 变体（或经生产路径间接验证）：
  - `chunked_init_orphan_rollback_test.go`（SetSessionTempPath）
  - `chunked_init_state_publish_test.go`（三处）
  - `chunked_init_takeover_publish_test.go` / `chunked_store_cleanup_atomic_test.go` / `chunked_store_lifecycle_test.go`（如有调用）

**注意：** 测试需持有会话对象（expect）才能调 IfCurrent——测试先用 `GetSession(uploadID)` 取会话再调门控变体。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 行尾纪律：改动后核查 `git ls-files --eol`（i/lf w/lf）。
- 提交前 `make prepare`。

---

### 任务 1：删除导出 setter + 测试改用门控变体

- [ ] **步骤 1：读三个导出 setter 定义（chunked_store.go:1474-1525）**

确认 `setSession*IfCurrent` 门控变体的签名（`publishSessionIfCurrent(expect, apply)` 需要会话对象）。

- [ ] **步骤 2：删除三个导出 setter**（保留 IfCurrent 变体 + publishSession 基础实现）

- [ ] **步骤 3：测试调用点改用门控变体**

```go
// 原：us.SetSessionRoute("pub-sid", "disk2", nil, nil, nil)
// 新：s := us.GetSession("pub-sid"); us.setSessionRouteIfCurrent(s, ...) // 包内测试可直接调
```

注意测试包：`chunked_*_test.go` 是 `package files`（同包）还是 `package files_test`？——同包可调私有 IfCurrent 变体。

- [ ] **步骤 4：验证**（go test ./pkg/files/... 全绿 + 变异：恢复导出 setter → 编译失败/测试红）

- [ ] **步骤 5：Commit**

```bash
git add pkg/files/chunked_store.go pkg/files/*_test.go
git commit -m "fix(files): 删除不门控导出 setter（SetSessionX 生产零调用，统一门控变体）" --no-verify
```

---

## 交付自检

- [ ] `gofmt -l` / `goimports -l` 无输出
- [ ] `go build ./...` + `make lint` 0 issues
- [ ] `go test ./pkg/files/...`（含 -race）全绿
- [ ] 变异验证：恢复任一导出 setter → 编译失败（删除被测试钉住）
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
