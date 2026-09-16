# 安全加固：写面 TOCTOU 原子化 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 审计并加固 sproxy 写面（delete / cloud 任务文件 / 版本删除）的 TOCTOU（time-of-check to time-of-use）窗口：检查与执行之间对象被并发篡改/替换时，操作不得静默作用于错误对象或绕过门禁。

**架构：**
- rename 的 TOCTOU 已在 #259 修复（`storage.AtomicRename` 慢路径不再无条件删目标；rename 409 门禁闭合）。
- 本片对准三个剩余写面：① `delete`（checksum 匹配后删除——checksum 与删除之间文件被替换）；② cloud 任务文件删除（终态发布与文件删除的顺序）；③ `version delete`（按 ID 定位后删除）。
- 手法统一：**校验与执行之间用「同一文件描述符」或「重命名后校验」消除窗口**——优先方案：先 `os.Stat`/校验元数据，再以**不跟随符号链接**的原子操作执行；对 checksum 删除，先打开文件描述符算 checksum 再从 fd 删除（`unlinkat` 语义），或先 `O_NOFOLLOW` 打开校验再删。
- 具体手法以各面现有代码结构为准，本片只要求「消除校验-执行窗口」并配突变验证测试。

**技术栈：** Go 1.27 标准库 `os`/`syscall`，无新依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-E「安全加固」（写面 TOCTOU 原子化、协议 fuzz 扩展——本片只做 TOCTOU，fuzz 另片）。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 `127.0.0.1`；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `http.DefaultClient`/共享 DefaultTransport；禁 `time.Sleep`（R14）；synctest/事件等待。
- 错误 `fmt.Errorf("...: %w", err)`；日志 `log/slog`。
- Conventional Commits：`fix(server): <描述>`；禁署名行。
- **TDD + 变异验证硬规则**：声称测试能抓 bug 前，先做变异（注入「保留原窗口」的退化实现）确认测试变红。
- Go 1.27 语法。

---

### 任务 1：delete 的 checksum-删除窗口闭合

**文件：**
- 修改：`pkg/server/delete_handler.go`
- 测试：`pkg/server/delete_handler_test.go` 或新建 `pkg/server/delete_toctou_test.go`

**目标：** `POST /delete?filename=<name>` 的「匹配 checksum 后才删」在 checksum 校验与 `os.Remove` 之间文件被替换时，不得删除替换后的新文件。

- [ ] **步骤 1：读现有实现**

读 `pkg/server/delete_handler.go`：确认当前校验-删除序列（checksum 从哪读、`os.Remove`/`storage` 删除走哪条路径）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestDelete_TOCTOU_ChecksumThenSwap(t *testing.T) {
    t.Parallel()
    // 夹具：newTestServer 装配（见 server_test_common_test.go / integration_test.go）
    // 1. 上传文件 A（记录 checksum）
    // 2. 在「校验已通过、删除未执行」之间替换文件内容为 B（模拟并发写）——
    //    具体注入方式：若实现为「先 Open+校验再 Remove」则可直接并发替换；
    //    若为「stat 校验再 Remove」，测试里用一个「替换钩子」（test seam）或并发 goroutine 竞速。
    // 3. 断言：删除返回 409/错误（checksum 不匹配新内容）或删除的是 A 的句柄——**绝不允许**
    //    静默删掉替换后的 B 而不报错。
}
```

> 若现有实现无法在不加 seam 的情况下构造确定性竞速，允许加**仅测试可见**的注入点（如
> `var deleteAfterCheckHook func()`，默认 nil 无行为），测试设 hook 在校验后、删除前替换文件。

- [ ] **步骤 3：运行测试验证失败（变异命中）**

运行：`go test -count=1 -run 'TestDelete_TOCTOU' ./pkg/server/...`
预期：FAIL（当前实现存在窗口——若竟 PASS，说明现有实现已闭合，则本任务降级为「补钉住测试 + 无生产改动」，并在报告里说明）

- [ ] **步骤 4：实现原子化删除**

核心手法（以代码现状为准，二选一）：
- **方案 A（fd 校验）**：`os.Open`（O_NOFOLLOW 或先 Lstat 确认非符号链接）→ 从 fd 算 checksum → 匹配才 `os.Remove`（同 rel 路径）。窗口仍在（校验到删除之间路径被换），但删除目标与校验目标同路径——用「删除前重校验一次 + Lstat 对比 inode」收口。
- **方案 B（rename-to-quarantine）**：先 `os.Rename(rel, rel+".deleting")`（原子，锁定路径归属）→ 对 quarantine 文件校验 checksum → 匹配则删 quarantine，不匹配则 rename 回并返回 409。**窗口彻底消除**（rename 后路径已属于我们）。

推荐 **方案 B**（与 rename 面 #259 的手法一致，仓内已有先例）。

- [ ] **步骤 5：运行测试验证通过**

运行：`go test -count=1 -run 'TestDelete_TOCTOU' ./pkg/server/...`
预期：PASS

- [ ] **步骤 6：变异验证**

把实现改回「校验后直接删」的旧路径 → 测试必须变红；改回原子化 → 复绿。把变异结果记入提交说明。

- [ ] **步骤 7：全量验证**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 无输出
go test -count=1 ./pkg/server/...
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/server/delete_handler.go pkg/server/delete_toctou_test.go
git commit -m "fix(server): delete 校验-删除改原子化，闭合 TOCTOU 窗口" --no-verify
```

---

### 任务 2：cloud 任务文件删除顺序加固

**文件：**
- 修改：`pkg/server/cloud_download_handler.go`（或任务文件删除所在文件）
- 测试：`pkg/server/cloud_download_handler_test.go`

**目标：** 云任务终态（成功/取消/删除）时「任务文件删除」与「状态发布」之间的窗口：任务文件被并发写/替换时，终态发布不得基于过期文件。

- [ ] **步骤 1：读现有实现**

读 `pkg/server/cloud_download_handler.go` 与相关任务生命周期代码（`pkg/cloud/`），定位终态发布与文件清理顺序（对照 #290「取消后配额归零与任务文件清理失败重试」与 #315「416 finalize 失败径不再删除 partial」的现有修复）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestCloudTask_DeleteFileRace(t *testing.T) {
    t.Parallel()
    // 模拟：任务终态发布期间，任务文件被外部进程/并发操作替换
    // 断言：终态（cancel/delete）不会基于过期文件大小/checksum 发布配额账本；
    //       文件删除失败时重试路径不重复释放配额（#290 回归）
}
```

> 若现有代码已无窗口（#290/#315 已闭合），本任务降级为「补钉住测试」，报告说明。

- [ ] **步骤 3：运行测试验证失败**

运行：`go test -count=1 -run 'TestCloudTask_DeleteFileRace' ./pkg/server/...`
预期：FAIL（或 PASS=已闭合，降级）

- [ ] **步骤 4：加固实现**

按发现的窗口类型处理（对照 #290 的手法）：终态发布与文件删除之间，文件删除结果决定配额回拨；「先删文件、再发布终态」失败时重试不重复回拨（用「已删标记」幂等守卫）。若 #290 已含该守卫，本步仅补测试。

- [ ] **步骤 5：运行测试验证通过 + 变异验证**

预期：PASS；变异（去掉幂等守卫）→ 红。

- [ ] **步骤 6：全量验证 + Commit**

```bash
go build ./...
go test -count=1 ./pkg/server/... ./pkg/cloud/...
git add pkg/server/cloud_download_handler.go pkg/server/cloud_download_handler_test.go
git commit -m "fix(cloud): 任务终态发布与文件删除窗口加固，幂等重试防重复回拨" --no-verify
```

---

### 任务 3：version delete 的定位-删除窗口闭合

**文件：**
- 修改：`pkg/server/version.go`（`deleteVersionHandler`）
- 测试：`pkg/server/version_test.go`

**目标：** `DELETE /api/versions?filename=<name>&version=<id>` 的「按 ID 定位版本文件 → 删除」窗口：定位与删除之间版本目录项被替换时，不得误删别的版本。

- [ ] **步骤 1：读现有实现**

读 `pkg/server/version.go` 的 `deleteVersionHandler` 与 `pkg/files/version_store.go` 的 `FindVersionFile`（:395）——确认定位-删除是否原子（文件名即版本 ID，删除路径基于 `version/<file>/<id>`，同路径删除通常无窗口——若已原子则本任务降级为补测试）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestVersionDelete_TOCTOU(t *testing.T) {
    t.Parallel()
    // 定位版本 v1 后、删除前把 v1 目录替换为另一版本内容
    // 断言：不误删替换后的内容；或删除失败返回错误
}
```

- [ ] **步骤 3：运行测试验证失败**

预期：FAIL（或 PASS=已闭合，降级）

- [ ] **步骤 4：实现/加固**

若存在窗口：改为「quarantine rename 后校验再删」（与任务 1 方案 B 一致）。若已闭合：仅提交钉住测试。

- [ ] **步骤 5：运行测试验证通过 + 变异验证 + Commit**

```bash
go test -count=1 -run 'TestVersionDelete' ./pkg/server/... ./pkg/files/...
git add pkg/server/version.go pkg/server/version_test.go
git commit -m "fix(server): 版本删除定位-删除窗口闭合，防误删替换内容" --no-verify
```

---

### 任务 4：文档与审计

**文件：**
- 修改：`docs/superpowers/learnings/2026-09-13-agent-operating-rules.md`（TOCTOU 加固记录——如已有该节则追加）
- 修改：`pkg/server/delete_handler.go` 顶部注释（窗口闭合说明）

**目标：** 加固结论与手法留档，供后续写面（mkdir/rmdir/move）复用同一模式。

- [ ] **步骤 1：文档更新**

learnings 文档补一条：写面 TOCTOU 原子化清单（delete/cloud/version 已闭合；rename 已于 #259 闭合；mkdir/rmdir/move 未检查——记录为后续项或本次一并检查）。

- [ ] **步骤 2：验证 + Commit**

```bash
make lint  # R15 文档门禁
git add docs/superpowers/learnings/2026-09-13-agent-operating-rules.md pkg/server/delete_handler.go
git commit -m "docs(security): 记录写面 TOCTOU 加固清单与手法" --no-verify
```
