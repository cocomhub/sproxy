# BaiduPCS 网盘 Plugin 实现计划 R2（fork+replace 方案）

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 提供百度网盘（BaiduPCS）存储 plugin，**不再 fork 裁剪核心库进 sproxy 仓库**——直接引用外部 fork（`github.com/cocomhub/BaiduPCS-Go`），经 `replace` 指令接入，规避 sproxy 强校验污染开源实现。

**架构（R2 方案，已实测验证）：**
- **外部 fork**：`github.com/cocomhub/BaiduPCS-Go`（fork qjfoidnh 上游，module 声明保持 `github.com/qjfoidnh/BaiduPCS-Go`，**go.mod 一行不改** → Sync fork 零冲突）。
- **sproxy 侧薄 adapter**：`pkg/baidupcs`（独立 module）只含**自有代码**（Adapter 接口、Storage 接口、plugin 注册、稳定性保障），import 上游 `github.com/qjfoidnh/BaiduPCS-Go/baidupcs` + `requester`。
- **replace 机制**（已实测：require 父 module + replace 指 fork commit；fork 内部 import 自引用 qjfoidnh/... 无需修改）：
  ```
  pkg/baidupcs/go.mod:
    module github.com/cocomhub/sproxy/pkg/baidupcs
    require github.com/qjfoidnh/BaiduPCS-Go v0.0.0
    replace github.com/qjfoidnh/BaiduPCS-Go => github.com/cocomhub/BaiduPCS-Go 1b9131817aaf8ca7dee24bc00e33ebc4c7a5cc73
  ```
- **执行策略**：二进制优先 + 库兜底双路径（cocom 验证模式）。
- **稳定性保障**（cocom 教训）：传输共享 client 无整体超时、上传后 ETag 复核有界重试 ≤3、错误分类映射、子进程超时。
- **plugin 注册**：经 `pkg/plugin.Registry[T]`；凭据（BDUSS）走凭据 Ring。

**技术栈：** Go 1.27；`GOWORK=off` 独立构建验证（AGENTS 硬规则 8）。

**规格：** 用户 2026-09-17 决策：百度网盘 plugin 放外部 fork（cocomhub/BaiduPCS-Go），sproxy replace 引用；不建 sproxy-contrib；保留上游名。

## 全局约束

- UTF-8 without BOM；SPDX 头（仅自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待；R14 误扫用变量拼接绕开（保持生成代码可编译）。
- 禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- **独立 module 硬规则**：`GOWORK=off` 独立构建/测试（AGENTS 硬规则 8）。
- 提交前 `make prepare`。

---

### 任务 1：pkg/baidupcs 独立 module 骨架 + NewClient

**文件：**
- 创建：`pkg/baidupcs/go.mod`（module cocomhub/sproxy/pkg/baidupcs + require qjfoidnh/BaiduPCS-Go + replace cocomhub fork）
- 创建：`pkg/baidupcs/baidupcs.go`（`NewClient(BDUSS, SToken)` 薄封装 + 路径工具）
- 创建：`pkg/baidupcs/README.md`（方案说明 + 维护流程）
- 测试：`pkg/baidupcs/baidupcs_test.go`

**目标：** module 可独立构建，引用 fork 核心库。

- [ ] **步骤 1：写 go.mod**

```go
// pkg/baidupcs/go.mod
module github.com/cocomhub/sproxy/pkg/baidupcs

go 1.27

require github.com/qjfoidnh/BaiduPCS-Go v0.0.0

replace github.com/qjfoidnh/BaiduPCS-Go => github.com/cocomhub/BaiduPCS-Go 1b9131817aaf8ca7dee24bc00e33ebc4c7a5cc73
```

> fork commit 用 `gh api repos/cocomhub/BaiduPCS-Go/commits/main --jq .sha` 实时取。

- [ ] **步骤 2：编写失败的测试**

```go
// pkg/baidupcs/baidupcs_test.go
func TestNewClient_RequiresBDUSS(t *testing.T) {
    t.Parallel()
    // 空 BDUSS → 错误
}
func TestSanitizeRemotePath(t *testing.T) {
    t.Parallel()
    // /a/b/ → /a/b；.. 拒绝；空 → /
}
```

- [ ] **步骤 3：运行测试验证失败** → 实现 `NewClient`（`bdlib.NewPCS(appID, bduss)` + SetStoken/SetPCSUserAgent）+ `sanitizeRemotePath` → 验证通过

```bash
cd pkg/baidupcs && GOWORK=off go mod tidy && GOWORK=off go build ./... && GOWORK=off go test ./...
```

> 首次 `go mod tidy` 会拉 fork 依赖（惰性加载只拉核心链，验证 go.sum 无 CLI 依赖）。

- [ ] **步骤 4：Commit**

```bash
git add pkg/baidupcs/go.mod pkg/baidupcs/go.sum pkg/baidupcs/baidupcs.go pkg/baidupcs/README.md pkg/baidupcs/baidupcs_test.go
git commit -m "feat(baidupcs): 独立 module 骨架，replace 引用外部 fork（零污染）" --no-verify
```

---

### 任务 2：Adapter 双路径（二进制优先 + 库兜底）

**文件：**
- 创建：`pkg/baidupcs/adapter.go`（Adapter 接口 + binaryAdapter + libraryAdapter）
- 测试：`pkg/baidupcs/adapter_test.go`

**目标：** 二进制优先、库兜底双路径。

- [ ] **步骤 1：编写失败的测试**

```go
// 用 fake BaiduPCS-Go 二进制（编译小 Go 程序，sleepCall 变量绕开 R14）
func TestBinaryAdapter_Upload_Success(t *testing.T)  { t.Parallel() /* fake bin 成功 → 走二进制 */ }
func TestBinaryAdapter_Upload_BinaryMissing_Fallback(t *testing.T) { t.Parallel() /* bin 不存在 → 回退库（fake pcs） */ }
func TestBinaryAdapter_Upload_Timeout_Fallback(t *testing.T) { t.Parallel() /* bin 超时 → 回退 */ }
func TestBinaryAdapter_Download_Retry(t *testing.T) { t.Parallel() /* 直链失败重试 */ }
```

- [ ] **步骤 2：运行测试验证失败** → 实现（复用 R1 分支的 adapter.go 思路：binaryAdapter exec + Fallback 接口；libraryAdapter 用 fork 的 PrepareUpload/DownloadFile）→ 验证通过 + 变异验证（去掉回退 → fallback 测试红）

- [ ] **步骤 3：Commit**

```bash
git add pkg/baidupcs/adapter.go pkg/baidupcs/adapter_test.go
git commit -m "feat(baidupcs): 二进制优先+库兜底双路径 Adapter" --no-verify
```

---

### 任务 3：Storage 接口 + 稳定性保障

**文件：**
- 创建：`pkg/baidupcs/storage.go`（Storage 全方法 + 稳定性）
- 创建：`pkg/baidupcs/errors.go`（哨兵错误 + PCSError 分类映射）
- 测试：`pkg/baidupcs/storage_test.go`

**目标：** 完整 Storage（Put/Get/Stat/List/Delete/Copy/Move/Exists）+ 稳定性保障。

- [ ] **步骤 1：编写失败的测试**

```go
func TestStorage_PutGetRoundtrip(t *testing.T)    { t.Parallel() }
func TestStorage_Stat_NotFound(t *testing.T)      { t.Parallel() }
func TestStorage_Put_ETagMismatch_Retries(t *testing.T) { t.Parallel() /* 有界重试 ≤3 */ }
func TestStorage_Get_TempCleanup(t *testing.T)    { t.Parallel() }
```

- [ ] **步骤 2：运行测试验证失败** → 实现（maxPutAttempts=3、mapPCSErrorCategory、共享 transferClient 无整体超时、Get 临时文件 Close 清理）→ 验证通过 + 变异验证（去掉有界重试 → ETag 测试红）

- [ ] **步骤 3：Commit**

```bash
git add pkg/baidupcs/storage.go pkg/baidupcs/errors.go pkg/baidupcs/storage_test.go
git commit -m "feat(baidupcs): Storage 接口实现，有界重试与错误分类保障稳定性" --no-verify
```

---

### 任务 4：plugin 注册 + 装配接线（片2）+ 文档

**文件：**
- 创建：`pkg/baidupcs/plugin.go`（`DefaultFactory` 注册）
- 修改：`cmd/sproxy/root.go`（配置 `storage.baidupcs.enabled` 时装配）
- 修改：`cmd/sproxy/go.mod`（require pkg/baidupcs + replace）
- 修改：`go.work`（use ./pkg/baidupcs）
- 修改：`README.md` / `docs/config.md`
- 测试：`pkg/baidupcs/plugin_test.go`

**目标：** plugin 可配置启用，文档同步。

- [ ] **步骤 1：编写失败的测试**

```go
func TestPluginRegistry_RegisterBaiduPCS(t *testing.T) {
    t.Parallel()
    // plugin.New[storageFactory] + Register + Active
}
```

- [ ] **步骤 2：运行测试验证失败** → 实现 plugin.go + 装配 → 验证通过

- [ ] **步骤 3：跨 module 接线**

```bash
# cmd/sproxy/go.mod 加
require github.com/cocomhub/sproxy/pkg/baidupcs v0.0.0
replace github.com/cocomhub/sproxy/pkg/baidupcs => ../../pkg/baidupcs
# go.work use 加 ./pkg/baidupcs
# GOWORK=off 独立验证 cmd/sproxy
cd cmd/sproxy && GOWORK=off go build ./... && GOWORK=off go test ./...
```

- [ ] **步骤 4：文档 + 全量验证 + Commit**

```bash
# 全量（GOWORK=on）
make prepare && go build ./... && go test ./pkg/... ./internal/... ./cmd/...
git add pkg/baidupcs/plugin.go cmd/sproxy/root.go cmd/sproxy/go.mod go.work README.md docs/config.md pkg/baidupcs/plugin_test.go
git commit -m "feat(baidupcs): plugin 注册与装配接线，配置启用百度网盘后端" --no-verify
```

> 本任务 = 片1（plugin 注册）+ 片2（cmd/sproxy 装配）合并，因 R2 方案 fork 在外部、sproxy 无污染，一次 PR 即可完成。
