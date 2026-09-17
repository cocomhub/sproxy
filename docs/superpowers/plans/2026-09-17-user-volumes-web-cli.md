# 用户卷 Web UI + sclient CLI 接入 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 用户卷的 CLI（`sclient volume create/list/delete`）+ Web UI（卷面板内「我的用户卷」区）接入，**完整可靠的 e2e 测试 + 浏览器自动化测试（Playwright）**。

**背景（用户确认 2026-09-17）：**
- ① 新建 `volume` 子命令；② Web 卷面板内新增「我的用户卷」区；③ `--extra` JSON（通用）
- 前置：用户卷 API 已合并（master `2f6cff6a`）：POST/GET/DELETE `/api/volumes/user`

**测试硬要求（用户明示）：**
- 纯函数 `node --test` 单测（新 JS 登记 Makefile `web-test`，R10 门禁）
- Playwright + Chromium 浏览器自动化（`web/e2e/`，必检项 UI E2E Tests）
- CLI e2e（真实二进制 + 子进程，`test/` 模式）

**架构：**
```
sclient volume create <name> --type baidupcs --extra '{"bduss":"..."}' [--capacity 100GiB]
sclient volume list
sclient volume delete <name>

Web：volumes-panel 内「我的用户卷」区（列表 + 创建弹窗 + 删除按钮）
  user-volumes.js（纯函数：userVolumesTableHtml/createFormHtml/parseExtra）+ fetch API
```

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(sclient): <描述>` / `feat(web): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **行尾纪律**：改动后核查 `git ls-files --eol`（i/lf w/lf）。
- **Web UI 硬规则**：纯函数补 `node --test` 单测；交互/渲染补 Playwright e2e；新 JS 登记 Makefile `web-test`（R10 门禁）。

---

### 任务 1：sclient volume 子命令（create/list/delete）+ 单元测试

**文件：**
- 新建：`cmd/sclient/volume.go`（NewCmdVolume：create/list/delete 子命令）
- 新建：`cmd/sclient/volume_test.go`（单元测试，CaptureStdout + fake client）
- 修改：`cmd/sclient/root.go`（挂载 volume 子命令）

**目标：** CLI 用户卷管理（create/list/delete）。

- [ ] **步骤 1：读现有子命令模式（cloud_download.go + volumes.go + clientfactory + cli.IOStreams）**

- [ ] **步骤 2：读 pkg/client 的 Volumes 方法（用户卷 API 客户端）——确认有无 CreateUserVolume/DeleteUserVolume**

（服务端 API 已有 POST/GET/DELETE /api/volumes/user——pkg/client 若缺则补：`CreateUserVolume` / `UserVolumes` / `DeleteUserVolume`）

- [ ] **步骤 3：编写失败的测试**

```go
func TestVolumeCreate(t *testing.T) { t.Parallel() /* create --type baidupcs --extra → 调用 API */ }
func TestVolumeList(t *testing.T)   { t.Parallel() /* list → 表格输出 */ }
func TestVolumeDelete(t *testing.T) { t.Parallel() /* delete <name> → 调用 API */ }
func TestVolumeCreate_BadExtra(t *testing.T) { t.Parallel() /* --extra 非 JSON → 错误 */ }
```

- [ ] **步骤 4：实现**（cobra 子命令 + --extra JSON 解析 + --capacity 人类可读 + 输出表格/--json）→ 验证通过

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/volume.go cmd/sclient/volume_test.go cmd/sclient/root.go
git commit -m "feat(sclient): volume 子命令（create/list/delete 用户卷管理）" --no-verify
```

---

### 任务 2：CLI e2e（真实二进制 + 用户卷 API 全链路）

**文件：**
- 新建：`test/user_volume_cli_e2e_test.go`（构建真实 sclient 二进制 + 启动 sproxy + 用户卷 API 全链路）

**目标：** CLI 全链路验证（create → list → delete 真实服务）。

- [ ] **步骤 1：读 test/e2e_test.go 的 startSPROXY 模式（构建真实二进制 + 子进程启动）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestUserVolumeCLI_CreateListDelete(t *testing.T) { t.Parallel() /* 真实服务：create → list 可见 → delete → list 消失 */ }
```

- [ ] **步骤 3：实现** → 验证通过（注意 e2e 配置隔离：--config 指向临时配置文件，防本机配置污染）

- [ ] **步骤 4：Commit**

```bash
git add test/user_volume_cli_e2e_test.go
git commit -m "test(sclient): volume 子命令 e2e（真实二进制 create/list/delete）" --no-verify
```

---

### 任务 3：Web UI 用户卷面板（纯函数 + 单测）

**文件：**
- 新建：`web/static/user-volumes.js`（纯函数：userVolumesTableHtml/createFormHtml/parseExtra）
- 新建：`web/static/user-volumes.test.js`（node --test 单测）
- 修改：`web/static/app.js`（showVolumes 增加「我的用户卷」区 + showUserVolumes/createUserVolume/deleteUserVolume）
- 修改：`web/static/index.html`（面板容器）
- 修改：`Makefile`（web-test 登记新 JS）

**目标：** Web 用户卷面板（渲染/创建/删除）。

- [ ] **步骤 1：读 app.js 的 showVolumes + appRender.volumesTableHtml 模式 + transfer-store.js 风格**

- [ ] **步骤 2：编写失败的测试（node --test）**

```js
// user-volumes.test.js
test('userVolumesTableHtml 渲染行', ...);
test('createFormHtml 含 name/type/extra/capacity 字段', ...);
test('parseExtra 解析 JSON / 非法报错', ...);
```

- [ ] **步骤 3：运行测试验证失败** → 实现 user-volumes.js（纯函数）→ 单测通过

- [ ] **步骤 4：app.js 接入**（showVolumes 内渲染「我的用户卷」区 + fetch POST/GET/DELETE + 交互）→ 手工/冒烟验证

- [ ] **步骤 5：登记 Makefile web-test**（R10 门禁：新 JS 必须登记）

- [ ] **步骤 6：Commit**

```bash
git add web/static/user-volumes.js web/static/user-volumes.test.js web/static/app.js web/static/index.html Makefile
git commit -m "feat(web): 用户卷面板（卷面板内「我的用户卷」区，创建/列表/删除）" --no-verify
```

---

### 任务 4：Playwright 浏览器自动化（user_volumes_e2e_test.go）

**文件：**
- 新建：`web/e2e/user_volumes_e2e_test.go`（Playwright + Chromium）

**目标：** 真实浏览器用户卷全链路（打开面板 → 创建 → 列表出现 → 删除 → 消失）。

- [ ] **步骤 1：读 web/e2e/files_e2e_test.go 的 Playwright 模式（testServer + page 交互）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestUserVolumesE2E_CreateListDelete(t *testing.T) { t.Parallel() /* 打开 /ui/ → 卷面板 → 创建用户卷（填 name/type/extra）→ 列表出现 → 删除 → 消失 */ }
func TestUserVolumesE2E_BadType(t *testing.T) { t.Parallel() /* 未注册 type → 错误提示 */ }
```

- [ ] **步骤 3：实现** → 验证通过（本地 Chromium 可跑：`make ui-e2e` 或直接 go test）

- [ ] **步骤 4：Commit**

```bash
git add web/e2e/user_volumes_e2e_test.go
git commit -m "test(web): 用户卷 Playwright e2e（浏览器创建/列表/删除）" --no-verify
```

---

### 任务 5：文档

**文件：**
- 修改：`cmd/sclient/README.md` 或 `docs/`（volume 子命令用法）
- 修改：`web/static/README.md` 或 docs（用户卷面板说明）

**目标：** CLI/Web 用户卷用法文档。

- [ ] **步骤 1：写文档**（volume create/list/delete 用法 + Web 面板说明 + extra JSON 示例）

- [ ] **步骤 2：Commit**

```bash
git commit -m "docs(sclient): volume 子命令 + Web 用户卷面板文档" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `make web-test`（node --test 全部通过，含新 user-volumes.test.js）
- [ ] `make ui-e2e` 或 `go test ./web/e2e/...`（Playwright 通过）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：parseExtra 去掉 → 单测红；delete 引用 409 分支 → e2e 红
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
