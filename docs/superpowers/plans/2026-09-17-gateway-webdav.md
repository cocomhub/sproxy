# WebDAV / 本地 HTTP 代理网关 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 提供 WebDAV 网关与本地 HTTP 代理，让任意工具（rsync/curl/编辑器/文件管理器）直接访问远端 `remote://<node>/<vol>/<path>`。

**架构：**
- `pkg/gateway/webdav`：`WebDAVHandler` 把 `sync.FS` 桥接为 WebDAV 协议（RFC 4918：PROPFIND/PROPPATCH/MKCOL/GET/PUT/DELETE/MOVE/COPY），基于 `golang.org/x/net/webdav`（准标准库，符合依赖策略）。
- 复用 `pkg/remote.Client.FS(ref)`（已返回 `sync.FS` 7 方法）作为 FS 来源。
- 挂载：sproxy 主 mux 加 `GET /remote-dav/`（鉴权走现有 authMiddleware）+ 独立子命令 `sproxy dav --listen 127.0.0.1:8080 remote://node/vol`（本地代理形态）。
- 本地 HTTP 代理 = 同一 WebDAV handler 的本地监听变体（`sproxy dav` 子命令绑定 loopback）。

**技术栈：** Go 1.27 + `golang.org/x/net/webdav`（x/net 是允许的准标准库）。

**规格：** 用户 2026-09-17 决策：WebDAV + 本地 HTTP 代理（先实现），FUSE 后续项。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待。
- 禁 `http.DefaultClient`/共享 DefaultTransport；每测试自建 `&http.Client{Transport:&http.Transport{}}`。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(gateway): <描述>`；禁署名行。
- 提交前 `make prepare`（embed 依赖）。
- Go 1.27 语法。

---

### 任务 1：WebDAV handler 核心

**文件：**
- 创建：`pkg/gateway/webdav/webdav.go`
- 测试：`pkg/gateway/webdav/webdav_test.go`

**目标：** `WebDAVHandler` 桥接 `sync.FS` 到 WebDAV。

- [ ] **步骤 1：读 x/net/webdav 用法**

`golang.org/x/net/webdav` 的 `Handler{FileSystem: FS, LockSystem: ...}`；`webdav.NewMemLS()` 内存锁系统；`Dir` 是内置 FS（映射到本地目录），但**不用** `webdav.Dir`——需要自定义 FS 适配器把 `sync.FS` 桥接过去（实现 `webdav.FileSystem` 接口：`Mkdir/MkdirAll`? 实际是 `Mkdir`? 查接口：`OpenFile(name, flag, perm)`、`RemoveAll(name)`、`Rename(oldName, newName)`、`Stat(name)`）。

- [ ] **步骤 2：编写失败的测试**

```go
// pkg/gateway/webdav/webdav_test.go
func TestWebDAVHandler_PropfindGetPut(t *testing.T) {
    t.Parallel()
    // 用内存 fake sync.FS（简单 map 实现）构造 handler
    // httptest.Server 挂 handler
    // 1. PUT /hello.txt（body "world"）→ 201
    // 2. GET /hello.txt → 200 "world"
    // 3. PROPFIND / → 207 Multi-Status 含 hello.txt
    // 4. MKCOL /dir → 201；PUT /dir/file.txt → 201
    // 5. DELETE /dir/file.txt → 204
    // 6. MOVE /dir/file.txt /moved.txt → 201（若已删则换文件）
}
func TestWebDAVHandler_StatMissing(t *testing.T) {
    t.Parallel()
    // GET /nope.txt → 404
}
```

> fake `sync.FS` 可用 `pkg/sync` 的现有测试夹具或自写 map 实现（`ListDir/Stat/OpenRead/WriteFile/Rename/Delete/MakeDir`）。

- [ ] **步骤 3：运行测试验证失败**

运行：`go test -count=1 -run TestWebDAVHandler ./pkg/gateway/webdav/`
预期：FAIL（handler 不存在）

- [ ] **步骤 4：实现**

```go
// webdavFS 把 sync.FS 桥接为 x/net/webdav 的 FileSystem。
type webdavFS struct{ fs sync.FS }
// 实现 webdav.FileSystem：
//   OpenFile(name, flag, perm) — 读模式走 fs.OpenRead；写模式走 fs.WriteFile（先收集到内存）
//   RemoveAll(name) — fs.Delete
//   Rename(old, new) — fs.Rename
//   Stat(name) — fs.Stat
//   Mkdir(name, perm) — fs.MakeDir
// 注：x/net/webdav 的 FileSystem 接口是 OpenFile/RemoveAll/Rename/Stat/Mkdir（查实际签名）
```

- [ ] **步骤 5：运行测试验证通过 + 变异验证（去掉某方法 → 对应测试红）**

运行：`go test -count=1 -run TestWebDAVHandler ./pkg/gateway/webdav/`
预期：PASS

- [ ] **步骤 6：全量验证 + Commit**

```bash
go build ./...
gofmt -l pkg/gateway/ goimports -l pkg/gateway/
go test -count=1 ./pkg/gateway/...
git add pkg/gateway/webdav/webdav.go pkg/gateway/webdav/webdav_test.go
git commit -m "feat(gateway): WebDAV handler 桥接 sync.FS，支持任意工具访问远端" --no-verify
```

---

### 任务 2：remote.FS 接入 + sproxy 主 mux 挂载

**文件：**
- 修改：`pkg/gateway/webdav/webdav.go`（新增 `NewRemoteWebDAVHandler`）
- 修改：`pkg/server/routes.go`（挂载 `/remote-dav/`）
- 测试：`pkg/gateway/webdav/remote_webdav_test.go`

**目标：** 把 `pkg/remote.Client.FS(ref)` 桥接进 WebDAV handler，挂 sproxy mux。

- [ ] **步骤 1：读 pkg/remote 的 Client.FS 用法**

`pkg/remote/fs.go:25` `func (c *Client) FS(ref Ref) syncpkg.FS`——用 `remote.NewClient(...)` 构造 + `FS(remote.Ref{Node, Vol, Path})` 拿到 `sync.FS`。

- [ ] **步骤 2：编写失败的测试**

```go
func TestRemoteWebDAVHandler_WithMockRemote(t *testing.T) {
    t.Parallel()
    // 用 pkg/testutil/mockserver 或自建 mock 远端（实现 sync.FS 的 fake）
    // 构造 remote.Client → FS(ref) → NewRemoteWebDAVHandler
    // httptest 挂载 → PROPFIND/GET/PUT 往返断言
}
```

- [ ] **步骤 3：运行测试验证失败** → 实现 `NewRemoteWebDAVHandler(ref, opts)`（内部 `remote.NewClient` + `FS(ref)` + `webdav.Handler`）→ 验证通过

- [ ] **步骤 4：挂 sproxy mux**

`routes.go` 加 `srvMux.Handle("GET /remote-dav/", h.authMiddleware(h.webdavHandler(ref)))`（ref 从配置/请求参数解析——本任务先用配置固定一个 ref 或从 `?remote=` 参数解析）。

> 简化：第一版 `GET /remote-dav/?remote=node/vol/path` 动态解析 ref；无 remote 参数返回 400。

- [ ] **步骤 5：全量验证 + Commit**

```bash
go build ./...
gofmt -l pkg/ goimports -l pkg/
go test -count=1 ./pkg/gateway/... ./pkg/server/...
git add pkg/gateway/webdav/webdav.go pkg/server/routes.go pkg/gateway/webdav/remote_webdav_test.go
git commit -m "feat(gateway): remote.FS 接入 WebDAV，挂 sproxy /remote-dav/ 端点" --no-verify
```

---

### 任务 3：本地代理子命令 `sproxy dav`

**文件：**
- 创建：`cmd/sproxy/dav.go`（cobra 子命令）
- 修改：`cmd/sproxy/root.go`（挂子命令）
- 测试：`cmd/sproxy/dav_test.go`

**目标：** `sproxy dav --listen 127.0.0.1:8080 remote://node/vol/path` 本地 HTTP 代理形态。

- [ ] **步骤 1：编写失败的测试**

```go
func TestDavCommand_BindsLoopback(t *testing.T) {
    t.Parallel()
    // 构造 cobra 命令，--listen 127.0.0.1:0（随机端口）
    // 启动后 GET / → 200（WebDAV 根）或 PROPFIND → 207
}
```

- [ ] **步骤 2：运行测试验证失败** → 实现 `newCmdDav`（解析 `remote://` ref → `NewRemoteWebDAVHandler` → `http.ListenAndServe`）→ 验证通过

- [ ] **步骤 3：全量验证 + Commit**

```bash
go build ./...
gofmt -l cmd/ goimports -l cmd/
go test -count=1 ./cmd/sproxy/...
git add cmd/sproxy/dav.go cmd/sproxy/root.go cmd/sproxy/dav_test.go
git commit -m "feat(gateway): sproxy dav 子命令，本地监听 WebDAV 代理" --no-verify
```

---

### 任务 4：文档 + 门禁

**文件：**
- 修改：`README.md`（WebDAV 用法）、`docs/config.md`（`/remote-dav/` 端点）

**目标：** 文档同步（R15 不漂移）。

- [ ] **步骤 1：更新文档**

README 补「WebDAV 网关」节：`curl -X PUT http://127.0.0.1:8080/hello.txt -d world`、`rsync -av /local/ dav://127.0.0.1:8080/`、`sproxy dav --listen` 用法。

- [ ] **步骤 2：验证 + Commit**

```bash
make lint
git add README.md docs/config.md
git commit -m "docs(gateway): WebDAV 网关与本地代理用法" --no-verify
```
