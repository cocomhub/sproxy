# WebDAV 存储后端（V3 plugin 第一个真实扩展） 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 实现 **WebDAV 存储后端**：把任意 WebDAV 服务（Nextcloud / 坚果云 / OwnCloud）作为 sproxy 外部卷——`sync.FS` 7 方法经 WebDAV 协议（RFC 4918）映射，经 V3 plugin（`RegisterBackend("webdav")`）接入系统盘（volumes[]）与用户卷。**验证 V3 核心承诺**：「新存储类型只 RegisterBackend 注册」。

**背景（用户确认 2026-09-17）：**
- ① WebDAV 放 `pkg/volume/webdav/`（格式类似 ext 子包，无新依赖无需 go.mod）
- ② syncmgr 新增 `kind=volume`（通用本机卷）；`kind=baidupcs` 保留兼容别名
- ③ Web UI backend 列表 API（GET /api/backends 动态下拉）
- 前置：V3 通用卷模型（master `c9cb2d2b`）：volume.Volume{Type/Extra} + registry external + RegisterBackend

**架构：**
```
pkg/volume/webdav/（用户定案：放 pkg/volume 下，格式类似 ext 子包；无新依赖无需 go.mod）
  webdav_fs.go    // WebDAVFS：sync.FS 7 方法（HTTP 客户端：PROPFIND/GET/PUT/MOVE/DELETE/MKCOL）
  webdav_fs_test.go

方法映射（RFC 4918）：
  ListDir(path)   → PROPFIND depth=1 → 子条目（name/size/mtime/isdir）
  Stat(path)      → PROPFIND depth=0 → 条目（不存在 → nil）
  OpenRead(path)  → GET → io.ReadCloser
  WriteFile(path, r, size, mtime) → PUT（PROPPATCH mtime 可选）
  Rename(from,to) → MOVE
  Delete(path)    → DELETE（404 → 幂等 OK）
  MakeDir(path)   → MKCOL（已存在 → 幂等 OK）

V3 plugin：
  RegisterBackend("webdav", newWebDAVBackend)
  v.Extra 读：url（必填）、username/password 或 token、local_root（可选中间态）
  → WebDAVFS 构造 → ExternalBackend{FS(), Close()}

syncmgr kind 通用化：
  新增 RemoteKindVolume = "volume"（通用本机卷：remote.volume 查 Set.External 任意类型）
  RemoteKindBaidupcs 保留兼容别名（旧配置零迁移；内部归一为 volume）

backend 列表 API：
  GET /api/backends → 已注册 backend 类型（registry 导出列表）
  Web UI 下拉动态 + sclient volume create 提示

中间态约束（用户硬规则）：staging/cache/tmp 落本地（local_root，与 baidupcs 一致）
```

## 全局约束

- UTF-8 without BOM；SPDX 头（自有代码）；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；禁 `http.DefaultClient`/共享 DefaultTransport（每测试自建 `&http.Client{Transport: &http.Transport{}}`）。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(volume): <描述>`；禁署名行。
- 提交前 `make prepare`。
- **行尾纪律**：改动后核查 `git ls-files --eol`（i/lf w/lf）。

---

### 任务 1：WebDAV 客户端（sync.FS 7 方法 + HTTP 客户端 + 认证）+ 单测

**文件：**
- 新建：`pkg/volume/webdav/webdav_fs.go`（WebDAVFS + 构造器 + 认证）
- 新建：`pkg/volume/webdav/webdav_fs_test.go`（httptest 模拟 WebDAV 服务端）

**目标：** WebDAVFS 实现 sync.FS 7 方法（RFC 4918 协议客户端）。

- [ ] **步骤 1：读 sync.FS 接口 + golang.org/x/net/webdav 包（协议知识）**

`pkg/sync/entry.go` 的 FS 接口 7 方法；x/net/webdav 的客户端工具（或手写 HTTP 方法）。

- [ ] **步骤 2：编写失败的测试（httptest 模拟 WebDAV 服务端）**

```go
func TestWebDAVFS_ListDir(t *testing.T)    { t.Parallel() /* PROPFIND depth=1 → 子条目 */ }
func TestWebDAVFS_Stat(t *testing.T)       { t.Parallel() /* PROPFIND depth=0；不存在 → nil */ }
func TestWebDAVFS_WriteRead(t *testing.T)  { t.Parallel() /* PUT → GET 往返 */ }
func TestWebDAVFS_Rename(t *testing.T)     { t.Parallel() /* MOVE */ }
func TestWebDAVFS_Delete(t *testing.T)     { t.Parallel() /* DELETE；404 幂等 */ }
func TestWebDAVFS_MakeDir(t *testing.T)    { t.Parallel() /* MKCOL；已存在幂等 */ }
func TestWebDAVFS_Auth(t *testing.T)       { t.Parallel() /* Basic/Bearer 认证头 */ }
```

- [ ] **步骤 3：运行测试验证失败** → 实现（WebDAVFS：http.Client + 根 URL + 认证；PROPFIND 解析 XML 响应；PUT/GET/MOVE/DELETE/MKCOL）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/webdav/webdav_fs.go pkg/volume/webdav/webdav_fs_test.go
git commit -m "feat(volume): WebDAV 客户端（sync.FS 7 方法，RFC 4918）" --no-verify
```

---

### 任务 2：WebDAV backend 插件（RegisterBackend + Extra 解析 + config 校验）

**文件：**
- 新建：`pkg/volume/webdav/backend.go`（newWebDAVBackend + RegisterBackend 注册）
- 修改：`cmd/sproxy/root.go`（装配段 registerWebDAVBackend）
- 修改：`pkg/server/config_validate.go`（volumes[] type=webdav 校验：extra.url 必填 + 认证）
- 测试：`pkg/volume/webdav/backend_test.go`

**目标：** WebDAV 成为 V3 可插拔 backend（系统盘 + 用户卷通用）。

- [ ] **步骤 1：读 registry.BackendFactory/ExternalBackend 签名 + baidupcs backend 模式（cmd/sproxy/baidupcs_sync.go）**

- [ ] **步骤 2：编写失败的测试**

```go
func TestNewWebDAVBackend_FromVolumeExtra(t *testing.T) { t.Parallel() /* Extra 读 url/username/password → WebDAVFS */ }
func TestNewWebDAVBackend_MissingURL(t *testing.T)      { t.Parallel() /* Extra 缺 url → 明确错误 */ }
func TestRegisterWebDAVBackend(t *testing.T)            { t.Parallel() /* registry.NewBackend 分派 */ }
```

- [ ] **步骤 3：实现**（newWebDAVBackend：Extra → url/认证 → WebDAVFS → ExternalBackend 包装；RegisterBackend("webdav")；config_validate type=webdav 校验）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/webdav/backend.go pkg/volume/webdav/backend_test.go cmd/sproxy/root.go pkg/server/config_validate.go
git commit -m "feat(volume): WebDAV backend 插件（RegisterBackend + config 校验）" --no-verify
```

---

### 任务 3：syncmgr kind 通用化（kind=volume + baidupcs 别名）

**文件：**
- 修改：`pkg/syncmgr/manager.go`（RemoteKindVolume = "volume"；RemoteKindBaidupcs 归一别名）
- 修改：`pkg/syncexec/executor.go`（newRemoteFS 支持 kind=volume → Set.External）
- 修改：`pkg/server/config_validate.go`（kind 白名单加 volume）
- 测试：`pkg/syncmgr/manager_remote_kind_test.go` + `pkg/syncexec/executor_remote_kinds_test.go`

**目标：** 本机卷 kind 通用化（WebDAV/baidupcs 统一走 kind=volume）。

- [ ] **步骤 1：读 RemoteKind 现状（direct/mesh/baidupcs）+ ValidateForTask + newRemoteFS**

- [ ] **步骤 2：编写失败的测试**

```go
func TestValidateRemote_KindVolume(t *testing.T) { t.Parallel() /* kind=volume + volume 非空 → OK */ }
func TestValidateRemote_KindBaidupcsAlias(t *testing.T) { t.Parallel() /* kind=baidupcs → 归一 volume 仍 OK */ }
func TestExecutor_Run_KindVolume(t *testing.T) { t.Parallel() /* 工厂查 Set.External（任意类型卷） */ }
```

- [ ] **步骤 3：实现**（RemoteKindVolume 常量；baidupcs 别名归一；newRemoteFS 支持 volume）→ 验证通过（旧 baidupcs 配置零回归）

- [ ] **步骤 4：Commit**

```bash
git add pkg/syncmgr/manager.go pkg/syncexec/executor.go pkg/server/config_validate.go pkg/syncmgr/manager_remote_kind_test.go pkg/syncexec/executor_remote_kinds_test.go
git commit -m "feat(syncmgr): kind=volume 通用本机卷（baidupcs 兼容别名）" --no-verify
```

---

### 任务 4：backend 列表 API + Web/CLI 动态下拉

**文件：**
- 修改：`pkg/volume/registry/backend.go`（`BackendTypes() []string` 导出已注册类型）
- 新建：`pkg/server/backends_api.go`（GET /api/backends handler）
- 修改：`pkg/server/routes.go`（挂路由，双侧注册）
- 修改：`web/static/sclient/api/files.js` + `web/static/app.js`（动态下拉）
- 修改：`cmd/sclient/volume.go`（create --type 提示已注册类型）
- 测试：`pkg/server/backends_api_test.go` + `web/static/user-volumes.test.js` 更新

**目标：** 前端动态感知已注册 backend（未来任何 backend 自动出现）。

- [ ] **步骤 1：读 registry backendFactories（导出列表）+ app.js type 下拉现状**

- [ ] **步骤 2：编写失败的测试**

```go
func TestBackendsAPI(t *testing.T) { t.Parallel() /* GET /api/backends → 已注册类型列表 */ }
```

- [ ] **步骤 3：实现**（registry.BackendTypes() + handler + 路由 + Web 动态下拉 + CLI 提示）→ 验证通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/registry/backend.go pkg/server/backends_api.go pkg/server/routes.go web/static/sclient/api/files.js web/static/app.js cmd/sclient/volume.go
git commit -m "feat(volume): backend 列表 API（GET /api/backends 动态下拉）" --no-verify
```

---

### 任务 5：e2e（真实 WebDAV 服务端 + 同步任务）+ 文档

**文件：**
- 新建：`pkg/volume/webdav/webdav_e2e_test.go`（httptest WebDAV 服务端 + sync 引擎 push/pull）或 `test/`
- 修改：`docs/cli.md` / `docs/api.md` / `README.md`（webdav 后端 + kind=volume + backend API）

**目标：** WebDAV 后端全链路验证（系统盘 + 用户卷 + 同步任务）+ 文档。

- [ ] **步骤 1：e2e**（httptest WebDAV 服务端 → RegisterBackend("webdav") → assembleVolumes（volumes[] type=webdav）→ Set.External → sync 引擎 push/pull 往返）

- [ ] **步骤 2：变异验证**（WebDAVFS.ListDir PROPFIND 去掉 → 测试红；kind 归一去掉 → 测试红）

- [ ] **步骤 3：文档**（webdav 配置示例 + kind=volume + backend 列表 API）

- [ ] **步骤 4：Commit**

```bash
git add pkg/volume/webdav/webdav_e2e_test.go docs/ README.md
git commit -m "test(volume): WebDAV 后端 e2e + 文档" --no-verify
```

---

## 交付自检（完成全部任务后）

- [ ] `gofmt -l` 与 `goimports -l` 无输出
- [ ] `go build ./...` + `make build-all`
- [ ] `make lint` + `make lint-all` 0 issues
- [ ] `go test ./pkg/... ./internal/...` + `make test-all`（含 -race）
- [ ] `make web-test`（node --test 全部通过）
- [ ] `GOWORK=off` 独立构建/测试（子 module）
- [ ] 变异验证：WebDAVFS 方法去掉 → 测试红；kind 归一去掉 → 测试红
- [ ] 行尾核查 `git ls-files --eol`（i/lf w/lf）
- [ ] 本地全绿后才 push 触发 CI（用户硬规则）
