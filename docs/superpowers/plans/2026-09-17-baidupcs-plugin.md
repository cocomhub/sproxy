# BaiduPCS 网盘 Plugin 实现计划（独立 go.mod 隔离）

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 提供百度网盘（BaiduPCS）存储 plugin，**独立 go.mod 隔离依赖**。参考 `D:\workdir\leon\cocomhub\cocom\pkg\storage\baidupcs` 的接入经验（用户已确认：库接入不稳、最终用二进制保证稳定），采用**二进制优先 + 库兜底**策略。

**架构：**
- **独立 module**：`pkg/baidupcs/`（自己 `go.mod`，`replace` 指向 sproxy 根 module），隔离 BaiduPCS-Go 的依赖（`baidu-tools/tieba` 等不污染主 module）。
- **核心库**：vendor/fork 裁剪 BaiduPCS-Go 的 `baidupcs` + `requester` 包（仅保留 API 客户端/上传/下载，裁 CLI 层）。
- **执行策略**（cocom 验证过的双路径）：
  1. **二进制优先**：`exec BaiduPCS-Go upload/download`（命令语义稳定）；二进制缺失/失败 → 回退
  2. **库兜底**：库 `PrepareUpload`/`DownloadFile` 裸 API（cocom 的 `uploadViaLibrary` 模式）
- **稳定性保障**（从 cocom 教训提炼）：
  - 传输用共享 `http.Client`（无整体超时，正文由 ctx 约束——cocom `newTransferClient` 教训）
  - 上传后 ETag 复核有界重试 ≤3（cocom `maxPutAttempts` 教训）
  - 错误分类映射集中维护（cocom `mapPCSErrorCategory` 模式：31066/-3/-9 → NotFound 等）
  - 子进程超时/看门狗（exec.CommandContext + 超时）
- **plugin 注册**：经 `pkg/plugin.Registry[T]` 注册为存储后端，与 `pkg/storage` 抽象对齐（cocom 的 `Storage` 接口：`Put/Get/Stat/List/Delete/Copy/Move`）。
- **凭据**：BDUSS/STOKEN 走凭据 Ring 加密存储（复用现有凭据机制）。

**技术栈：** Go 1.27；独立 go.mod 用 `GOWORK=off` 构建验证；BaiduPCS-Go 源码 fork 裁剪（Apache-2.0，与本仓一致）。

**规格：** 用户 2026-09-17 决策：暂缓对象存储，但支持百度网盘 plugin，参考/改造 BaiduPCS-Go；独立 go.mod 隔离依赖；详细评估二进制稳定性。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待。
- 禁 `http.DefaultClient`/共享 DefaultTransport；每测试自建 client。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(baidupcs): <描述>`；禁署名行。
- **独立 module 硬规则**：新增跨 module 依赖要补 `require`+`replace`，并在 **`GOWORK=off`** 下独立构建/测试通过（sproxy AGENTS.md 硬规则 8）。
- 提交前 `make prepare`。
- Go 1.27 语法。

---

### 任务 1：独立 module 骨架 + 核心库 fork

**文件：**
- 创建：`pkg/baidupcs/go.mod`（module `github.com/cocomhub/sproxy/pkg/baidupcs`，replace 指向根 module）
- 创建：`pkg/baidupcs/baidupcs.go`（薄封装：`NewClient(BDUSS, SToken) *Client`）
- 创建：`pkg/baidupcs/README.md`（fork 说明 + 裁剪清单 + 二进制/库双路径策略）
- 测试：`pkg/baidupcs/baidupcs_test.go`

**目标：** module 骨架可独立构建。

- [ ] **步骤 1：fork 核心库**

把 BaiduPCS-Go 的 `baidupcs` + `requester` 包源码**手工裁剪**到 `pkg/baidupcs/internal/`（只保留 API 客户端/上传/下载，裁 CLI/配置/更新/展示层）。**不直接 import 上游**（用户明确：独立 go.mod 隔离依赖）。

> 工作量提示：`baidupcs` 包 4082 行 + `requester` 若干子包。**本任务先做最小可编译裁剪**（NewPCS + FilesDirectoriesList/Meta + PrepareUpload + DownloadFile + Remove/Mkdir/Rename/Move），其余按需补。

- [ ] **步骤 2：写 go.mod**

```go
// pkg/baidupcs/go.mod
module github.com/cocomhub/sproxy/pkg/baidupcs

go 1.27

require (
    github.com/cocomhub/sproxy v0.0.0
    // 裁剪后内部依赖（如有第三方则列出）
)

replace github.com/cocomhub/sproxy => ../..
```

- [ ] **步骤 3：编写失败的测试**

```go
// pkg/baidupcs/baidupcs_test.go
func TestNewClient_RequiresBDUSS(t *testing.T) {
    t.Parallel()
    // 空 BDUSS → 错误
}
func TestSanitizeRemotePath(t *testing.T) {
    t.Parallel()
    // 路径归一：/a/b/ → /a/b；.. 拒绝
}
```

- [ ] **步骤 4：运行测试验证失败 → 实现 `NewClient` + 路径工具 → 验证通过**

```bash
cd pkg/baidupcs && GOWORK=off go build ./... && GOWORK=off go test ./...
```

- [ ] **步骤 5：Commit**

```bash
git add pkg/baidupcs/go.mod pkg/baidupcs/internal/ pkg/baidupcs/baidupcs.go pkg/baidupcs/README.md pkg/baidupcs/baidupcs_test.go
git commit -m "feat(baidupcs): 独立 module 骨架 + BaiduPCS 核心库 fork 裁剪" --no-verify
```

---

### 任务 2：Adapter 抽象 + 二进制优先执行

**文件：**
- 创建：`pkg/baidupcs/adapter.go`（Adapter 接口 + libraryAdapter + binaryAdapter）
- 测试：`pkg/baidupcs/adapter_test.go`

**目标：** 双路径执行：二进制优先、库兜底。

- [ ] **步骤 1：编写失败的测试**

```go
// 用 fake BaiduPCS-Go 二进制（shell 脚本模拟 upload/download 成功/失败）
func TestBinaryAdapter_Upload_Success(t *testing.T) { t.Parallel() /* fake bin 成功 → 走二进制 */ }
func TestBinaryAdapter_Upload_BinaryMissing_Fallback(t *testing.T) { t.Parallel() /* bin 不存在 → 回退库（fake pcs 接口） */ }
func TestBinaryAdapter_Download_Retry(t *testing.T) { t.Parallel() /* 直链失败 → 重试 */ }
```

- [ ] **步骤 2：运行测试验证失败 → 实现**

```go
type Adapter interface {
    Meta(path string) (*FileDirectory, error)
    List(path string) ([]FileDirectory, error)
    Delete(paths ...string) error
    Copy(entries ...*CpMvJSON) error
    Move(entries ...*CpMvJSON) error
    Upload(ctx, localPath, targetPath string, overwrite bool) error
    Download(ctx, remotePath, localPath string) error
}

// binaryAdapter：exec BaiduPCS-Go（命令路径可配置，默认查找 PATH）
// libraryAdapter：fork 的库 API（PrepareUpload/DownloadFile）
// 顶层：先 binary，失败（二进制缺失/超时/退出码非零）→ library
```

- [ ] **步骤 3：验证通过 + 变异验证（去掉回退 → fallback 测试红）+ Commit**

```bash
cd pkg/baidupcs && GOWORK=off go test ./...
git add pkg/baidupcs/adapter.go pkg/baidupcs/adapter_test.go
git commit -m "feat(baidupcs): 二进制优先+库兜底双路径 Adapter" --no-verify
```

---

### 任务 3：Storage 接口实现 + 稳定性保障

**文件：**
- 创建：`pkg/baidupcs/storage.go`（实现 sproxy 侧 Storage 接口）
- 修改：`pkg/baidupcs/README.md`
- 测试：`pkg/baidupcs/storage_test.go`

**目标：** 完整 Storage 实现（Put/Get/Stat/List/Delete/Copy/Move），含 cocom 稳定性保障。

- [ ] **步骤 1：编写失败的测试**

```go
// fake adapter（内存 map）驱动 Storage 接口全方法
func TestStorage_PutGetRoundtrip(t *testing.T)  { t.Parallel() }
func TestStorage_Stat_NotFound(t *testing.T)    { t.Parallel() }
func TestStorage_List_Recursive(t *testing.T)   { t.Parallel() }
func TestStorage_Delete_Missing(t *testing.T)   { t.Parallel() }
func TestStorage_Put_ETagMismatch_Retries(t *testing.T) { t.Parallel() /* 上传后 ETag 复核不匹配 → 有界重试 ≤3 */ }
```

- [ ] **步骤 2：运行测试验证失败 → 实现（复用 cocom 的 baidupcs.go 模式：maxPutAttempts=3、mapPCSErrorCategory、transferClient 共享无整体超时）**

- [ ] **步骤 3：验证通过 + 变异验证（去掉有界重试 → ETag 测试红）+ Commit**

```bash
cd pkg/baidupcs && GOWORK=off go test ./...
git add pkg/baidupcs/storage.go pkg/baidupcs/storage_test.go pkg/baidupcs/README.md
git commit -m "feat(baidupcs): Storage 接口实现，有界重试与错误分类保障稳定性" --no-verify
```

---

### 任务 4：plugin 注册 + 装配接线 + 文档

**文件：**
- 修改：`pkg/baidupcs/plugin.go`（经 `pkg/plugin.Registry[T]` 注册）
- 修改：`cmd/sproxy/root.go` 或装配点（配置 `storage.baidupcs.enabled` 时装配）
- 修改：`README.md` / `docs/config.md`
- 测试：`pkg/baidupcs/plugin_test.go`

**目标：** plugin 可经配置启用，文档同步。

- [ ] **步骤 1：编写失败的测试**

```go
func TestPluginRegistry_RegisterBaiduPCS(t *testing.T) {
    t.Parallel()
    // plugin.Registry 注册 baidupcs 后端 → Active() 返回它
}
```

- [ ] **步骤 2：运行测试验证失败 → 实现 plugin.go（Register 到 `pkg/plugin`）+ 装配（配置开关）→ 验证通过**

- [ ] **步骤 3：文档 + 全量验证（含 GOWORK=off 独立构建）+ Commit**

```bash
cd pkg/baidupcs && GOWORK=off go build ./... && GOWORK=off go test ./...
cd /d/workdir/leon/cocomhub/sproxy && make prepare && go build ./... && go test ./pkg/... ./internal/... ./cmd/...
git add pkg/baidupcs/plugin.go cmd/sproxy/root.go README.md docs/config.md pkg/baidupcs/plugin_test.go
git commit -m "feat(baidupcs): plugin 注册与装配接线，配置启用百度网盘后端" --no-verify
```

> 注意：`pkg/baidupcs` 是独立 module，根 module 的 `go.work` 需加 `pkg/baidupcs`（或按 sproxy 现有子模块惯例处理——查 `go.work` 现状）。子 module 改动需 `GOWORK=off` 独立验证（AGENTS 硬规则 8）。
