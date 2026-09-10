# PR-F2：Web UI 交互式浏览器 E2E 补强 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。每个任务建议：全新 implementer 子代理 + 独立 reviewer 子代理；任务内先写红灯（用例先失败于「未接线」）、再确认它只在真实接线存在时通过。

**目标：** 把 `web/e2e` 从「元素存在性 / false-green」断言升级为**真交互断言**——每个用例走「点击/输入控件 → 捕获并断言网络请求（URL/query/header/body）→ 断言 DOM 副作用（变化后的文本/行数/元素出现-消失）」，覆盖 12 个 Web UI 流程，并把 `web/e2e` 内 lint 归零。

**架构：** 全部新增/改写为 `web/e2e` 包内的 `*_e2e_test.go`（独立 module，`replace ../../`，可在 Go 侧 import `pkg/otp`、`pkg/server` 等根 module 包）。共用现有 helper（`testServer` / `pageFixture` / `seedUploadToVolume` / `waitResponse` / `fileExists`），并新增一个基建文件提供 `testServerCfg` 配置注入、dialog 处理器、locator 等待/网络 body 捕获辅助。不断言「元素存在」；凡 dialog 流程必先装 `OnDialog`；凡轮询流程留 ≥10s。

**技术栈：** Go 1.26（`web/e2e` module）、`github.com/mxschmitt/playwright-go v0.6100.0`（headless Chromium）、`net/http/httptest`（server 与本地云下载文件源均在 127.0.0.1）、`encoding/base32` + `github.com/cocomhub/sproxy/pkg/otp`（TOTP 用例）。纯标准库断言（不上 testify）。

---

## 现场事实（已核验，实现者直接采信）

### A. 现有 helper 逐字签名（`web/e2e/` 包内全部可见，新文件直接复用）

- `testServer(t *testing.T, opts ...bool) (string, *server.Config, func())`（`ui_e2e_test.go:25`）：`cfg := server.Default()`；`cfg.StorageRoot = t.TempDir()`；`cfg.LogLevel = "error"`；`cfg.CredentialTTL = -1`；`cfg.AllowInsecureLoopback = true`；`opts[0]==true` → `Versioning.Enabled=true; MaxVersions=10`。经 `server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{Mux, CfgPtr, Version:"e2e-test", BuildAt:"e2e-test", Logger: discard})` + `httptest.NewServer`。cleanup 关 ts/handler + `RemoveAll(<tmpDir>/.__cloud__)`、`RemoveAll(<tmpDir>/.__downloads__)`。
- `testFile(t *testing.T, uploadsDir, name, content string)`（`ui:67`）：直写 `<uploadsDir>/<name>`（多租户 `anonymous/user` 桶）。
- `userRoot(cfg *server.Config) string`（`ui:79`）：`<cfg.StorageRoot>/anonymous/user`。
- `pageFixture(t *testing.T) (playwright.Page, func())`（`ui:84`）：headless Chromium；`playwright.Run()` 失败 `t.Skipf`。
- `multiVolumeTestServer(t *testing.T) (string, map[string]string, func())`（`volumes:56`）：双卷 main/disk2。
- `volFile(t *testing.T, roots map[string]string, vol, rel, content string)`（`volumes:107`）。
- `seedUploadToVolume(t *testing.T, baseURL, vol, filename string, content []byte) (int, string)`（`volumes:116`）：真 multipart `POST /upload`，`volume` 普通字段 + `file` 文件件 + `X-File-Checksum` SHA-256，返回 (状态, 响应体)。
- `waitResponse(t *testing.T, req playwright.Request) playwright.Response`（`volumes:156`）：轮询 `req.Response()` 5s@25ms。
- `fileExists(p string) bool`（`volumes:169`）。
- **默认单卷名 = `"default"`**（`pkg/server/config.go:498/586/729`）：`server.Default()` 的 `Volumes=[{Name:"default", Root: defaultStorageRoot}]`；`resolveDefaultVolumeRoot` 在 Root 为占位值时裁决为 `cfg.StorageRoot`。故 `seedUploadToVolume(t, baseURL, "default", name, content)` 在默认 `testServer` 上是**真实上传路径**，可直接用于版本/分享/删除用例的 seed。

### B. Playwright API（本 module 实际可用，已核验）

- `page.OnDialog(func(Dialog))`（**不是** `On`）；`Dialog.Accept(promptText ...string) error` / `Dismiss() error` / `Message() string` / `Type() string`。**无 `Once`**——单次 dialog 也用 `OnDialog`（处理器常驻）。
- `page.ExpectRequest(url any, cb func() error, options ...PageExpectRequestOptions) (Request, error)` / `page.ExpectResponse(...)`；`Promise` 用 glob 串（如 `"**/api/files?*"`）。
- `Request.URL() string` / `Method() string` / `PostDataBuffer() ([]byte, error)` / `Headers() map[string]string`（键小写）。
- `Response.Status() int` / `JSON(v any) error` / `Body() ([]byte, error)`。
- `Locator.Click()/Fill()/SetInputFiles(any)/SelectOption(SelectOptionValues)/Count()/InnerText()/InputValue()/IsVisible()/First()/Filter(LocatorFilterOptions{HasText})`。
- **`Page.WaitForSelector` 已废弃**（`generated-interfaces.go:4354` `Deprecated: Use ... Locator.WaitFor instead`）→ lint SA1019。迁移到 **`Locator(sel).First().WaitFor(LocatorWaitForOptions{State, Timeout})`**：`Locator.WaitFor` 内部 `Strict: Bool(true)`，多匹配选择器（如 `#file-table tr`、`text=xxx`）必须先 `.First()`。
- `WaitForSelectorStateVisible` **是 `*WaitForSelectorState`**（与 `Attached/Hidden` 同类型）。

### C. UI 接线链（控件 → JS → API → DOM，逐条核验）

| 流程 | 控件 / 触发 | 网络（方法 + 路径 + 关键字段） | DOM 副作用 |
|---|---|---|---|
| 上传 | `#file-input` `SetInputFiles` → change → `setVolumeContext` + `uploadFiles` | `POST /upload`；`X-File-Checksum`(sha256 hex)、`X-File-Path`、`X-File-MTime` 头；multipart `file` + 可选 `volume` | `#upload-progress-container` 出现 → 成功 toast → `refreshList` → `#file-table` 新行；磁盘落 `userRoot(cfg)/<name>` |
| mkdir | `#new-dir-name` + `#mkdir-btn`（**无 dialog**） | `POST /mkdir?dirname=<enc>` | toast → `refreshList` → `.dir-row[data-subdir]` 出现 |
| 面包屑 | `.dir-enter-btn`（`data-subdir`）→ `navigateDir` | `GET /api/files?subdir=<enc>&offset=0&limit=500` | `#dir-breadcrumb` 文本含子目录；`#file-table` 换内容 |
| 单删 | `.file-delete-btn`（`data-filename`/`data-checksum`）→ `confirm` | `POST /delete?filename=<enc>`；头 `X-File-Checksum` | 行消失（`refreshList` 后） |
| 批量删 | `.file-select` 勾选 → `#batch-delete-btn` → `confirm` | `POST /api/batch/delete`；body `{"files":[{"filename","checksum"}]}` | `#file-table tr` 计数归零 |
| rename | `.file-rename-btn` → `prompt` | `POST /rename?from=<enc>&to=<enc>`；头 `X-File-Checksum` | 新名行出现、旧名消失 |
| 批重命名 | 勾选 → `#batch-rename-btn` → 逐个 `prompt` | `POST /api/batch/rename`；body `{"operations":[{"from","to","checksum"}]}` | 新名可见 |
| rmdir | `.dir-delete-btn` → `confirm` | `POST /rmdir?dirname=<enc>&force=true` | `.dir-row` 消失 |
| 分享 | `.file-share-btn` → `#share-create-btn` | `POST /api/share`；body `{"filename","ttl":"24h","max_downloads":0,"one_time":false}`；列表 `GET /api/shares`；撤销 `DELETE /api/shares/{token}` | `#share-list-body` 行内 `.share-copy-btn[data-token]` / `.share-revoke-btn[data-token]` |
| 公链 | 非 UI：`GET /s/{token}` | 服务端 `accessShareHandler` octet-stream + `Content-Disposition: attachment` | Go 侧 HTTP 取 body 断言字节 |
| 版本 | `#version-btn` → fill `#version-filename` → `#version-load-btn` | `GET /api/versions?filename=<enc>`；恢复 `POST /api/versions/restore?filename=&version_id=`；删除 `DELETE /api/versions?filename=&version_id=` | `#version-body` 表格行（`.version-restore-btn[data-version-id]`）；禁用时 501 → `#version-body` 文案（非空表） |
| 云下载 | `#cloud-btn` → fill `#cloud-url` → `#cloud-submit-btn` → **preview** → `#cloud-preview-confirm-btn` | `POST /api/cloud/download`；body `{"url","filename"}`（多 URL 走 `/api/cloud/download/batch` `{"urls":[{url,filename}]}`）；轮询 `GET /api/cloud/tasks`（每 3s） | preview 替换 `#cloud-url` 行为 `.cloud-preview-filename` 输入 + 确认按钮；`#transfer-body` 出现任务行（状态文案）；行操作 `.cloud-cancel-btn`/`.cloud-resume-btn`/`.cloud-remove-btn[data-id]` |
| 审计 | `#stats-btn` → `#audit-tab` | `GET /api/audit?limit=200` | `#audit-panel` 表格（`appRender.auditTableHtml`；列入 `时间/操作/主体/mesh/对象/结果/详情`）或 `空态 .empty-msg` |
| 配置 | `#stats-btn` → `#config-tab` → fill `#cfg-max-storage` → `#cfg-update-storage` | 打开 `GET /api/config`；更新 `PUT /api/config` body `{"max_storage_bytes":N}` | `showConfig` 重新 `GET /api/config` 重建面板 → `#cfg-max-storage` `value==N`；toast「配置已更新」 |
| 卷 | `#upload-volume` `change` → `setVolumeContext`；`GET /api/volumes` 填充 option | `GET /api/volumes` | option 列表；`currentVolume()` 前端状态 |

**关键服务端/配置事实（影响用例开关）：**
- **无凭据模型自包含红线**：`CredentialTTL=-1` 使启动时 ring 为空；`handleNoCredentials`（`pkg/server/auth.go:473`）仅在 `ring 空` 且请求来自 loopback 且 `AllowInsecureLoopback` 时合成 anonymous Principal（**放行全部方法**）。**一旦某用例注册了凭据（ring 非空），该 server 实例后续所有匿名 loopback 请求一律 401** → 注册/登录用例必须**独立 `testServer` 实例 + 自包含**。
- **TOTP 注册**：`cfg.Registration.ForceTOTP`（`pkg/server/config.go:284`，yaml `force_totp`，默认 false）；true 时 `POST /api/credentials/register` 返回 `{ak, owner, admin, otpauth_uri, base32_secret}`（**无 sk**）→ 登录流程 `POST /api/credentials/nonce` → `POST /api/credentials/login {ak,nonce,code,login_type:"web"}`。默认 `testServer` 不设此开关，**需新增 `testServerCfg` 注入**。
- **云下载 SSRF**：`downloader/ssrf.go:90-93` 硬拒 `127.0.0.1`/`localhost`；仅当 `cfg.CloudDownloadAllowPrivate=true`（`config.go:466` → `handlers.go:737` → `cloudMgr.AllowPrivate`，默认 **false**，`config.go:561`）才放行回环源 → 云下载用例**需 `testServerCfg` 注入此开关**，用 `httptest` 本地文件源。
- **审计 ring 默认开**：`AuditConfig.BufferSize` 默认 2048（`config.go:547`），`server.Default()` 已带；`PUT /api/config` **无条件**记一条 `config_update` 成功事件（`config_api.go:235`），`RecordAudit` 的 `Actor` 取自 ctx Principal（`audit.go:47`；anonymous 回环无 AK → Actor 空，DOM 渲染 `-`）。故审计用例可用 Go 侧 `PUT /api/config` 做**确定性 seed**。
- **版本落盘**：`upload_handler.go:342` / `chunked_upload.go:78,937` 在 `Versioning.Enabled` 时对**覆盖写**产版本 → 同文件名**两次真上传**（`seedUploadToVolume`）得非空版本表。
- **默认卷名 `"default"`**（见 A）。
- **`/api/volumes/move` 无 UI 入口**（仅 sclient CLI）→ 本 PR **不**为其写 UI e2e。
- **无 `/api/storage/config` 路由**（已由 `PUT /api/config` 取代）→ 不得为其写断言。

### D. lint 现状（必须归零）

`web/e2e` 内 `GOWORK=off golangci-lint run -c ../../.golangci.yml ./...` 实测 **8 issues**：
- `govet shadow` ×5：`ui_e2e_test.go:205`(TestAuthFlow `err`)、`432`&`437`(TestCloudDownloadCreateTask)、`624`(TestVersioningModalOpenClose)、`783`(TestStorageConfigInStats)。
- `SA1019` deprecated `WaitForSelector` ×3（flag 前 3；全文件约 20 处，完整修须迁 `Locator.WaitFor`）。
- **根 `make lint` / CI `lint` job（`golangci-lint-action` args `./...`）扫不到 `web/e2e`（嵌套 module）**。PR-F1 的 `test-submodules`/`test-e2e` job 与 web/e2e lint 接线**尚未合入当前 master**（`ci.yml` 无这两个 job；`Makefile SUB_MODULE_DIRS` 明确 `-not -path './web/e2e/*'`）。故本 PR 的 lint 门禁表述为：**本地必须 0 issues；CI 侧随 PR-F1 CI 接线生效**（若 F1 先落地，则本 PR 的 e2e 文件受其 lint job 门控）。

---

## 红线（每个任务的 DoD 共同部分）

1. **真交互断言，非存在性**：每个用例 = 「控件操作 → `ExpectRequest`/`ExpectResponse` 断言 URL/query/header/body → `WaitFor`/`InnerText`/`Count` 断言**变化后的** DOM」。
2. **禁 false-green**：禁止只断元素存在；禁止断言 click 前就已可见的元素；禁止断 `.empty-msg`（预置占位）这类「载入前即存在」的节点；禁止停在中途（如只点到 preview）。
3. **dialog 流程必先 `OnDialog`**：`confirm`/`prompt` 无人处理时 Playwright 默认 auto-dismiss → 点击 inert。所有 confirm/prompt 用例先 `acceptDialog`/自定义 responder。
4. **轮询类 ≥10s**：云下载 `transfer` 每 3s 轮询 → 断言 `Timeout ≥ Float(10000)`。
5. **监听一律 127.0.0.1**（`httptest` 默认）；**禁用** `0.0.0.0`/`localhost`。
6. **注册/登录用例自包含**（独立 server 实例，见 A/B 事实）。
7. **`web/e2e` lint 0 issues**（含既有 8 issues 的修复）。
8. 所有新增 Go 文件带 SPDX 头（`make fmt`/`addlicense` 强制）；UTF-8 无 BOM。

---

## 文件结构

| 文件 | 新增/修改 | 职责 |
|---|---|---|
| `web/e2e/helpers_e2e_test.go` | 新增 | **基建**：`testServerCfg`（配置注入）、`acceptDialog`/`respondDialogs`、`waitLoc`、`waitTextGone`/`waitTextVisible`、`requestJSON`、`seedConfigUpdate`、TOTP 解码/算码小工具 |
| `web/e2e/files_e2e_test.go` | 新增 | 任务 1+2：上传 / mkdir / 面包屑 / 搜索；单删 / 批删 / rename / 批重命名 / rmdir |
| `web/e2e/share_version_e2e_test.go` | 新增 | 任务 3：分享创建+公链+撤销；版本列表/恢复/禁用 |
| `web/e2e/cloud_audit_e2e_test.go` | 新增 | 任务 4：云下载（含 preview→confirm、完成、删除、取消）；审计面板 |
| `web/e2e/auth_config_e2e_test.go` | 新增 | 任务 5：TOTP 注册+登录、auth-bar 保存+签名、配置面板更新、单卷下拉补漏 |
| `web/e2e/ui_e2e_test.go` | 修改 | 任务 0：迁 `WaitForSelector`→`Locator.WaitFor`、修 shadow；删除/搬迁 false-green 用例（`TestAuthFlow` 陈旧 localStorage、`TestVersioningLoadVersions`、`TestVersioningDisabledMessage`、`TestCloudDownloadCreateTask`、`TestShareButton` 死 fallback）；保留有意义的「用户操作入口存在性」用例 |
| `.github/workflows/ci.yml` | 修改 | 抬升 `ui-e2e` job 的 `-timeout=120s`（新增 ~25 个含浏览器启动的用例，建议 `600s`） |

> 不新建 `*_test.go` 之外的任何包；不引入第三方测试库。

---

### 任务 0：测试基建增强 + `web/e2e` lint 归零

**文件：**
- 新增：`web/e2e/helpers_e2e_test.go`
- 修改：`web/e2e/ui_e2e_test.go`（lint + false-green 清退）
- 修改：`.github/workflows/ci.yml`（ui-e2e timeout）

**复用 helper：** 现有 `testServer` 签名**逐字保留**（既有调用点不改）。

- [ ] **步骤 1：`testServer` 抽出可配置变体，签名逐字保留**

在 `helpers_e2e_test.go` 实现 `testServerCfg`，并把 `ui_e2e_test.go` 的 `testServer` 改为**薄委托**（同包可跨文件调用）：

```go
// testServerCfg 启动 sproxy 测试实例，允许调用方在启动前修改 cfg——用于
// ForceTOTP=true（TOTP 注册）、CloudDownloadAllowPrivate=true（回环云下载源）、
// 自定义 Volumes 等。无凭据前提与 testServer 完全一致（CredentialTTL=-1 +
// AllowInsecureLoopback=true），保证既有用例语义零回归。
func testServerCfg(t *testing.T, mutate func(cfg *server.Config)) (string, *server.Config, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := server.Default()
	cfg.StorageRoot = tmpDir
	cfg.LogLevel = "error"
	cfg.CredentialTTL = -1
	cfg.AllowInsecureLoopback = true
	if mutate != nil {
		mutate(cfg)
	}
	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "e2e-test", BuildAt: "e2e-test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(h.Handler())
	return ts.URL, cfg, func() {
		ts.Close()
		h.Close()
		os.RemoveAll(filepath.Join(tmpDir, ".__cloud__"))
		os.RemoveAll(filepath.Join(tmpDir, ".__downloads__"))
	}
}
```

`ui_e2e_test.go` 的 `testServer` 改为（**签名逐字不变**，`opts ...bool` 语义不变，所有既有调用点零改动）：

```go
func testServer(t *testing.T, opts ...bool) (string, *server.Config, func()) {
	t.Helper()
	return testServerCfg(t, func(cfg *server.Config) {
		if len(opts) > 0 && opts[0] {
			cfg.Versioning.Enabled = true
			cfg.Versioning.MaxVersions = 10
		}
	})
}
```

要点：`testServerCfg` 的启动/清理体必须与现 `testServer` **逐字等价**（同 `RegisterRoutesOpts`、同 `RemoveAll` 两个 `.__` 目录），否则回归。

- [ ] **步骤 2：新增 dialog / 等待 / 网络辅助**

```go
// respondDialogs 安装 page 级 dialog 处理器（常驻）。Playwright 默认 auto-dismiss，
// 未装处理器时 confirm() 返回 false → 点击 inert；所有 confirm/prompt 流程必须先装。
// 序列 dialog（批重命名 N 个 prompt、恢复的 2 个 confirm）在 respond 内按 d.Message()
// / d.Type() 分派，或用闭包计数（Playwright 串行派发 dialog 事件，如跨 goroutine 计数
// 请用 sync.Mutex 保护）。
func respondDialogs(page playwright.Page, respond func(d playwright.Dialog)) {
	page.OnDialog(func(d playwright.Dialog) { respond(d) })
}

// acceptDialog 对每个 dialog 一律 Accept（prompt 时写入 promptText）。
func acceptDialog(page playwright.Page, promptText string) {
	respondDialogs(page, func(d playwright.Dialog) { _ = d.Accept(promptText) })
}

// waitLoc 用 locator-based 等待替代已废弃的 Page.WaitForSelector（strict=false：
// First() 取首个，等价旧 Page.WaitForSelector 的多匹配语义）。
func waitLoc(page playwright.Page, selector string, state *playwright.WaitForSelectorState, timeoutMs float64) error {
	return page.Locator(selector).First().WaitFor(playwright.LocatorWaitForOptions{
		State: state, Timeout: playwright.Float(timeoutMs),
	})
}

// waitTextGone 轮询 #file-list 等容器的 InnerText，直到不再包含 want（≤timeout）。
// 用于断言删除/重命名后的行消失（避免只断元素存在）。
func waitTextGone(t *testing.T, page playwright.Page, sel, want string, timeoutMs float64) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		txt, err := page.Locator(sel).InnerText()
		if err == nil && !strings.Contains(txt, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("文本 %q 未在 %.0fms 内从 %s 消失（疑似未接线）", want, timeoutMs, sel)
}

// requestJSON 解析请求体 JSON 到 v。
func requestJSON(t *testing.T, req playwright.Request, v any) {
	t.Helper()
	b, err := req.PostDataBuffer()
	if err != nil {
		t.Fatalf("读取请求体: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("解析请求体 JSON: %v (body=%q)", err, string(b))
	}
}
```

（file 内 import：`encoding/json`、`strings`、`time`、`path/filepath`、`os`、`io`、`log/slog`、`net/http`、`net/http/httptest`、`sync/atomic`、`github.com/cocomhub/sproxy/pkg/server`、`github.com/mxschmitt/playwright-go`。）

- [ ] **步骤 3：迁移既有 `ui_e2e_test.go` 的全部 `Page.WaitForSelector`**

逐个替换（约 20 处）：
- `page.WaitForSelector(sel, PageWaitForSelectorOptions{State: Visible, Timeout: Float(8000)})` → `waitLoc(page, sel, playwright.WaitForSelectorStateVisible, 8000)`（其它 state 同理）。
- `text=hello.txt` 这类文本选择器 → `page.Locator("text=hello.txt").First().WaitFor(...)`（strict 规避）。
- 需要元素句柄的地方改用 `Locator(...)` 直接操作，不再依赖 `ElementHandle`。
核验：`grep -n "WaitForSelector(" web/e2e/*.go` 应**无 `Page.WaitForSelector` 调用**（`waitLoc` 内部使用 `Locator.WaitFor`）。

- [ ] **步骤 4：修 5 处 `govet shadow`**

`ui:205`（`TestAuthFlow` 内层 `if _, err := ...` 与外层 `raw, err :=` 重名）、`432`&`437`（`TestCloudDownloadCreateTask` 内外层 `err`）、`624`（`TestVersioningModalOpenClose`）、`783`（`TestStorageConfigInStats`）——按 `:=` → `=` 或改写作用域消除 shadow。

- [ ] **步骤 5：清退 false-green 用例（改写/删除）**

- `TestAuthFlow`：断言陈旧 `localStorage sproxy_token`（真实 key 是 `sessionStorage sproxy_access_key/_secret/_id`）→ **删除**（其价值由任务 5 `TestAuth_SaveKeysSigns` 的 sessionStorage + 签名请求断言取代）。
- `TestVersioningLoadVersions`、`TestVersioningDisabledMessage`：`.empty-msg` 载入前即存在 → **删除**（由任务 3 网络+表格行断言取代）。
- `TestCloudDownloadCreateTask`：停在 preview、且 `#transfer-body` 本就可见 → **删除**（由任务 4 完成链路取代）。
- `TestShareButton`：fallback 引用**已删全局** `window.shareFile`（死路径）→ **删除**（由任务 3 取代）。
- 保留并保留其存在性语义：`TestUILoads`/`TestFileList`/`TestDirectoryNavigation`/`TestUploadButton`/`TestDownloadLink`/`TestBatchToolbar`/`TestStatsPanel`/`TestMkdirButton`/`TestCloudDownloadButtonExists`/`TestCloudDownloadModalOpens`/`TestCloudDownloadModalCloses`/`TestCloudDownloadURLInput`/`TestVersioningButton`/`TestDirArchiveButton`/`TestSearchFunction`/`TestStorageConfigInStats`/`TestStorageConfigAPI`/`TestHubTab`（均为「静态入口存在/弹窗开关」类，保留无妨；`TestStorageConfigAPI` 是直 fetch，保留但**不**作为配置接线证据，任务 5 另立 UI 用例）。

- [ ] **步骤 6：抬升 ui-e2e job 超时**

`.github/workflows/ci.yml` 的 `ui-e2e` job `go test -v -count=1 -timeout=120s ./...` → `-timeout=600s`（新增用例每例启动一次 headless Chromium + 8–10s 等待，120s 会 panic）。

- [ ] **步骤 7：lint / 编译 / 回归验证**

运行（Git Bash，Windows 下 `GOWORK=off` 必须）：
```bash
cd web/e2e
GOWORK=off golangci-lint run -c ../../.golangci.yml ./...   # 期望 0 issues
GOWORK=off go vet ./...
GOWORK=off go test -count=1 -run 'TestUILoads|TestFileList' ./...   # 冒烟
```

**DoD：** `web/e2e` lint 0 issues；`testServer` 签名与既有调用点零改动、既有用例全绿；新增 `testServerCfg`/`acceptDialog`/`waitLoc`/`waitTextGone`/`requestJSON` 可用。

---

### 任务 1：无 dialog 组（上传 / mkdir / 面包屑 / 搜索）

**文件：** 新增 `web/e2e/files_e2e_test.go`（本节部分）
**复用：** `testServer`、`userRoot`、`testFile`、`seedUploadToVolume`、`pageFixture`、`waitLoc`、`waitTextGone`、`fileExists`
**配置依赖：** 默认 `testServer`（无需开关）

- [ ] **TestFiles_Upload** — 上传链路
  - 控件：`page.Locator("#file-input").SetInputFiles([]string{probe})`（`probe` 为 `t.TempDir()` 内小文件，内容已知）。
  - 网络：`req, err := page.ExpectRequest("**/upload", func() error { return page.Locator("#file-input").SetInputFiles([]string{probe}) }, playwright.PageExpectRequestOptions{Timeout: playwright.Float(10000)})`。断言：
    - `req.Method() == "POST"`；
    - `req.Headers()["x-file-checksum"] == hex(sha256(content))`（Go 侧用 `crypto/sha256` 预算）；
    - `body, _ := req.PostDataBuffer()`；`bytes.Contains(body, []byte("name=\"file\""))` 且 `bytes.Contains(body, []byte(filepath.Base(probe)))`；
    - 默认单卷：body **不含** `name="volume"`（`currentVolume()==""` 时 `volume` 为 undefined）。
  - DOM/a 磁盘：`waitLoc(page, "#file-table tr", Visible, 8000)`；`page.Locator("#file-table").InnerText()` 含 `filepath.Base(probe)`；`fileExists(filepath.Join(userRoot(cfg), filepath.Base(probe)))`。
  - 反 false-green：断言**上传后**才出现的行（seed 前 `#file-list` 为空）。

- [ ] **TestFiles_Mkdir** — 新建目录（无 dialog）
  - 控件：fill `#new-dir-name` = `"newdir"`；`ExpectRequest("**/mkdir?dirname=*", click #mkdir-btn)`。
  - 网络：`req.URL()` 含 `dirname=newdir`（`url.QueryUnescape` 或 `strings.Contains`）。
  - DOM：`waitLoc(page, ".dir-row[data-subdir='newdir']", Visible, 8000)`；以及 `#dir-breadcrumb` 仍为根（文本 `/`）。反 false-green：断言 `.dir-row` **计数 0→1**（先断言 `page.Locator(".dir-row").Count()==0`）。

- [ ] **TestFiles_Breadcrumb** — 目录导航 + 返回根
  - seed：`testFile(t, userRoot(cfg), "sub/deep.txt", "deep")`。
  - 进入子目录：`resp, err := page.ExpectResponse("**/api/files?*", func() error { return page.Locator(".dir-row[data-subdir='sub'] .dir-enter-btn").Click() }, {Timeout: Float(8000)})`；断言 `resp.URL()` 含 `subdir=sub`。
  - DOM：`#dir-breadcrumb` `InnerText()` 含 `sub`；`waitLoc(page, "#file-table tr", Visible, 8000)` 且 `#file-list` 文本含 `deep.txt`。
  - 返回根：`resp2, err := page.ExpectResponse("**/api/files?*", func() error { return page.Locator("#dir-breadcrumb a[data-subdir='']").Click() }, {Timeout: Float(8000)})`；断言 `resp2.URL()` **不含** `subdir=sub`；`waitTextGone(t, page, "#file-list", "deep.txt", 8000)`；`.dir-row[data-subdir='sub']` 重新出现。
  - 反 false-green：`#dir-bar` 存在（旧用例）不作为证据；以 URL query 变化 + 列表内容变化为准。

- [ ] **TestFiles_Search** — 搜索（强化现有）
  - seed：`testFile(t, userRoot(cfg), "search-me.txt", "...")` + `testFile(t, userRoot(cfg), "other.txt", "...")`。
  - fill `#search-input` = `"search-me"`；`ExpectRequest("**/api/files/search?*", click #search-btn)`；断言 `req.URL()` 含 `q=search-me`。
  - DOM：`#file-list` 文本含 `search-me.txt` **且不含** `other.txt`（证明是搜索结果而非全量列表）。
  - 清除：`page.Locator("#clear-search-btn").Click()` 后 `#clear-search-btn` `IsVisible()==false`（`style.display='none'`），且 `#file-list` 重含 `other.txt`。

**DoD：** 四例均「点击/输入 → 断言请求 URL/头/体 → 断言变化后的 DOM」；无 `WaitForSelector`；lint 0；失败时错误信息能定位未接线点。

---

### 任务 2：dialog 组（单删 / 批删 / rename / 批重命名 / rmdir）

**文件：** `web/e2e/files_e2e_test.go`（续）
**复用：** `acceptDialog`/`respondDialogs`、`seedUploadToVolume`（真上传，checksum 存储就绪）、`waitTextGone`、`testFile`、`userRoot`
**配置依赖：** 默认 `testServer`

- [ ] **TestFiles_Delete** — 单文件删除
  - seed：`status, body := seedUploadToVolume(t, baseURL, "default", "del-me.txt", []byte("x"))`；断言 `status==200`；Go 预算 `sum := sha256.Sum256([]byte("x"))`。
  - dialog：`acceptDialog(page, "")`（confirm → 接受）。**必须在点击前装**。
  - 控件+网络：`req, err := page.ExpectRequest("**/delete?filename=*", func() error { return page.Locator(".file-delete-btn[data-filename='del-me.txt']").Click() }, {Timeout: Float(8000)})`；断言：
    - `req.Method()=="POST"`、`req.URL()` 含 `filename=del-me.txt`；
    - `req.Headers()["x-file-checksum"] == hex(sum[:])`。
  - DOM：`waitTextGone(t, page, "#file-list", "del-me.txt", 8000)`；`fileExists(listed path)==false`。

- [ ] **TestFiles_BatchDelete** — 批量删除
  - seed：`seedUploadToVolume(... "b1.txt" ...)`、`... "b2.txt" ...`（各 200 OK）。
  - 勾选：`page.Locator(".file-select[data-filename='b1.txt']").Check()`、`b2.txt` 同理（`Locator.Check()`；或 `.Click()`）。断言 `#batch-count` 文本含 `已选 2`。
  - dialog：`acceptDialog(page, "")`。
  - 控件+网络：`req := ExpectRequest("**/api/batch/delete", click #batch-delete-btn)`；`requestJSON(t, req, &struct{ Files []struct{ Filename, Checksum string } }{})`；断言 `len(Files)==2` 且文件名集合 == {b1.txt, b2.txt} 且各 `Checksum` 非空（等于对应用户已知 sha256）。
  - DOM：`waitTextGone(#file-list, "b1.txt")` + `waitTextGone(#file-list, "b2.txt")`；最终 `page.Locator("#file-table tr").Count()==0`。

- [ ] **TestFiles_Rename** — 重命名（prompt）
  - seed：`seedUploadToVolume(... "old-name.txt" ...)`。
  - dialog：`acceptDialog(page, "new-name.txt")`（prompt → Accept 带文本）。
  - 控件+网络：`req := ExpectRequest("**/rename?*", click `.file-rename-btn[data-filename='old-name.txt']`)`；断言 `req.URL()` 含 `from=old-name.txt` 且 `to=new-name.txt`；`Headers()["x-file-checksum"]` 非空。
  - DOM：`#file-list` 文本含 `new-name.txt`；`waitTextGone(#file-list, "old-name.txt", 8000)`。

- [ ] **TestFiles_BatchRename** — 批量重命名（N 个 prompt，序列 dialog）
  - seed：`r1.txt`、`r2.txt`（各真上传）。
  - 勾选两者（`.file-select` `Check()`）。
  - dialog：`respondDialogs(page, respond)`，respond 内按 `d.Message()` 分派：消息含 `r1.txt` → `d.Accept("n1.txt")`；含 `r2.txt` → `d.Accept("n2.txt")`；兜底 `d.Accept("")`。（Playwright 串行派发 dialog，分派安全。）
  - 控件+网络：`req := ExpectRequest("**/api/batch/rename", click #batch-rename-btn)`；`requestJSON` 断言 `operations` 含 `{from:"r1.txt",to:"n1.txt"}` 与 `{from:"r2.txt",to:"n2.txt"}` 且各 `checksum` 非空。
  - DOM：`#file-list` 文本含 `n1.txt`、`n2.txt`；`waitTextGone` 旧名。

- [ ] **TestFiles_Rmdir** — 删除目录（confirm）
  - seed：`testFile(t, userRoot(cfg), "rmdir-me/x.txt", "x")`（目录行 `.dir-row[data-subdir='rmdir-me']`）。
  - dialog：`acceptDialog(page, "")`。
  - 控件+网络：`req := ExpectRequest("**/rmdir?*", click `.dir-row[data-subdir='rmdir-me'] .dir-delete-btn`)`；断言 `req.URL()` 含 `dirname=rmdir-me` **且 `force=true`**。
  - DOM：`waitTextGone(#file-list, "rmdir-me", 8000)`（行消失）。

**DoD：** 五例的 dialog 均在点击前装；每例都断言了请求方法/URL/关键 header 或 body；DOM 断言基于**变化**（行消失/新名出现/计数归零）。lint 0。

---

### 任务 3：分享 + 公链 + 版本

**文件：** 新增 `web/e2e/share_version_e2e_test.go`
**复用：** `seedUploadToVolume`、`testServerCfg`（版本用例）、`acceptDialog`、`waitLoc`
**配置依赖：** 分享用默认 `testServer`；版本用 `testServerCfg(func(cfg){ cfg.Versioning.Enabled=true; cfg.Versioning.MaxVersions=10 })`（等价 `testServer(t,true)`，但显式以示范新基建）

- [ ] **TestShare_CreateAndPublicAccess** — 分享创建 + 公链
  - seed：`status, _ := seedUploadToVolume(t, baseURL, "default", "share-src.txt", []byte("shareable body"))`；断言 `status==200`。
  - goto → `waitLoc("#file-table tr", Visible, 8000)` → 点 `.file-row` 的 `.file-share-btn[data-filename='share-src.txt']`（`showShareModal` 设置 `#share-filename` 并 `switchShareTab('create')`）。
  - DOM（点击后）：`#share-modal` `IsVisible()==true`；`#share-filename` `InputValue()=="share-src.txt"`。
  - 控件+网络：`req := ExpectRequest("**/api/share", click #share-create-btn, Timeout 8000)`；断言 `req.Method()=="POST"`；`requestJSON` 得 `{filename:"share-src.txt", ttl:"24h", max_downloads:0, one_time:false}`（逐字段断言）。
  - 取 token：等待 `#share-list-body .share-copy-btn` 出现；`token, _ := page.Locator("#share-list-body .share-copy-btn").First().GetAttribute("data-token")`；断言 `token != ""`。
  - 公链（Go 侧，最强行证）：`resp, err := http.Get(baseURL + "/s/" + token)`；断言 `resp.StatusCode==200`；`Content-Disposition` 含 `attachment`；`io.ReadAll(resp.Body)` 字节 == `"shareable body"`。
  - 撤销：`acceptDialog(page, "")`；`req2 := ExpectRequest("**/api/shares/*", click `#share-list-body .share-revoke-btn`, Timeout 8000)`；断言 `req2.Method()=="DELETE"`；DOM：`#share-list-body` 文本含 `暂无分享链接`（`refreshShareList` 后行消失）。

- [ ] **TestVersioning_UploadCreatesVersions** — 版本列表 + 恢复
  - server：`baseURL, cfg, cleanup := testServerCfg(t, func(c *server.Config) { c.Versioning.Enabled = true; c.Versioning.MaxVersions = 10 })`。
  - seed 两版（真上传，覆盖写产版本）：`seedUploadToVolume(t, baseURL, "default", "versioned.txt", []byte("v1 content"))` → 断言 200；`seedUploadToVolume(..., "versioned.txt", []byte("v2 content"))` → 断言 200。
  - goto → 点 `#version-btn` → `waitLoc("#version-modal", Visible, 8000)` → fill `#version-filename` = `"versioned.txt"`。
  - 控件+网络：`resp, err := page.ExpectResponse("**/api/versions?*", click #version-load-btn, Timeout 8000)`；断言 `resp.Status()==200`；`resp.JSON(&payload)` 解析 `versions`；断言 `len(versions) >= 1`（覆盖写至少留 1 版）。
  - DOM（**非 `.empty-msg`**）：`waitLoc("#version-body table tbody tr", Visible, 8000)`；`page.Locator("#version-body .version-restore-btn").Count() >= 1`；`#version-body` `InnerText()` 含 `共 `（`buildVersionTableHtml` 头部文案）。
  - 恢复：`acceptDialog(page, "")`；`req := ExpectRequest("**/api/versions/restore?*", click `#version-body .version-restore-btn`, Timeout 8000)`；断言 `req.URL()` 含 `filename=versioned.txt` 且 `version_id=`（非空）。
  - 反 false-green：先断言 `showVersioning()` 初始化时 `#version-body` 含「输入文件名查看版本历史」，**加载后**该占位消失、表格行出现。

- [ ] **TestVersioning_DisabledReturns501** — 版本禁用
  - server：默认 `testServer`（无版本）。
  - goto → `#version-btn` → fill `#version-filename`=`any.txt` → `resp := ExpectResponse("**/api/versions?*", click #version-load-btn)`；断言 `resp.Status()==501`；DOM：`#version-body` 文本含 `加载失败`（`loadVersions` catch）且 `page.Locator("#version-body table").Count()==0`。
  - 反 false-green：以**网络状态码 501** 为主证据，DOM 为辅。

**DoD：** 分享三链（创建/列表 token/公链取字节/撤销）皆真交互；版本以「两次真上传 → 网络返回版本 → 表格行 → 恢复请求」闭环，杜绝 `.empty-msg` 假绿。lint 0。

---

### 任务 4：云下载 + 审计

**文件：** 新增 `web/e2e/cloud_audit_e2e_test.go`
**复用：** `testServerCfg`、`testServer`、`pageFixture`、`waitLoc`、`acceptDialog`、`seedConfigUpdate`（新增）
**配置依赖：** 云下载 `testServerCfg(func(c *server.Config){ c.CloudDownloadAllowPrivate = true })`；审计默认 `testServer`

- [ ] **步骤 1：新增本地文件源 helper（`helpers_e2e_test.go`）**

```go
// startFileSource 启动一个 127.0.0.1 的 httptest 文件源，返回可见 URL 与 cleanup。
// 用于云下载 e2e（cloud_download_allow_private=true 才放行回环源）。
func startFileSource(t *testing.T, name string, content []byte) (url string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	})
	ts := httptest.NewServer(mux) // 默认 127.0.0.1
	return ts.URL + "/" + name, ts.Close
}

// startStallingSource 启动一个「挂住不返回」的源：handler 阻塞到 stop() 被调用，
// 使云任务停在 downloading 状态，供取消（cancel）用例断言。
func startStallingSource(t *testing.T, name string) (url string, stop func()) {
	t.Helper()
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
		<-done // 直到 cleanup 才解除阻塞
	})
	ts := httptest.NewServer(mux)
	return ts.URL + "/" + name, func() { close(done); ts.Close() }
}

// seedConfigUpdate 用真实 HTTP PUT /api/config 触发一条 config_update 审计事件
// （config_api.go 无条件 RecordAudit）。返回状态码。loopback + 无凭据兜底放行。
func seedConfigUpdate(t *testing.T, baseURL string) int {
	t.Helper()
	body := strings.NewReader(`{"log_level":"info"}`)
	req, err := http.NewRequest(http.MethodPut, baseURL+"/api/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seed config update: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
```

- [ ] **TestCloudDownload_SubmitCompleteRemove** — 提交→preview→confirm→完成→删除
  - server：`testServerCfg(t, func(c *server.Config){ c.CloudDownloadAllowPrivate = true })`；源：`srcURL, stopSrc := startFileSource(t, "cloud-src.bin", bytes.Repeat([]byte("c"), 1024))`（`defer stopSrc()`）。
  - goto → 点 `#cloud-btn` → `waitLoc("#transfer-page", Visible, 8000)`。
  - fill `#cloud-url` = `srcURL`；点 `#cloud-submit-btn`（**进入 preview，非提交**）。
  - DOM（preview 证据）：`waitLoc("#cloud-preview-confirm-btn", Visible, 8000)`；且 `#transfer-page` 内出现 `.cloud-preview-filename` 输入，`InputValue()=="cloud-src.bin"`（`safeDefaultFromURL`）。
  - 控件+网络：`req := ExpectRequest("**/api/cloud/download", click #cloud-preview-confirm-btn, Timeout 8000)`；断言 `req.Method()=="POST"`；`requestJSON` 得 `{url: srcURL, filename:"cloud-src.bin"}`（**url 字段等于源 URL**，证明前端把输入交给 API）。
  - 完成（轮询 ≥10s）：`waitLoc("#transfer-body", Visible, 10000)`；轮询直到 `page.Locator("#transfer-body").InnerText()` 含 `cloud-src.bin` **且**含 `已完成`（≤10s，每 250ms 轮询；用 `waitTextVisible` helper）。随后 Go 侧 `GET /api/cloud/tasks` 断言存在 `{filename:"cloud-src.bin", status:"completed"}`。
  - 删除：`req2 := ExpectRequest("**/api/cloud/tasks/*", click `.cloud-remove-btn`（在含 cloud-src.bin 的行内）, Timeout 8000)`；断言 `req2.Method()=="DELETE"`；DOM `waitTextGone(#transfer-body, "cloud-src.bin", 10000)`。

- [ ] **TestCloudDownload_Cancel** — 取消（挂起源）
  - server：同上；源：`srcURL, stopSrc := startStallingSource(t, "stall.bin")`（`defer stopSrc()`）。
  - goto → `#cloud-btn` → fill `#cloud-url` = `srcURL` → `#cloud-submit-btn` → `ExpectRequest("**/api/cloud/download", click #cloud-preview-confirm-btn)`。
  - 行出现：`waitTextVisible(t, page, "#transfer-body", "stall.bin", 10000)`（状态可能为 等待中/下载中）。
  - 控件+网络：`req := ExpectRequest("**/api/cloud/tasks/*/cancel", click `.cloud-cancel-btn`, Timeout 8000)`；断言 `req.Method()=="POST"` 且 URL 以 `/cancel` 结尾。
  - DOM（≤10s）：`waitTextVisible(t, page, "#transfer-body", "已取消", 10000)`。

- [ ] **TestAudit_RendersSeededEvent** — 审计面板
  - server：默认 `testServer`；seed：`if st := seedConfigUpdate(t, baseURL); st != 200 { t.Fatalf("seed config_update status=%d", st) }`（Go 侧 HTTP，先于 goto）。
  - goto → 点 `#stats-btn` → `waitLoc("#stats-modal", Visible, 8000)`。
  - 控件+网络：`resp, err := page.ExpectResponse("**/api/audit?limit=200", func() error { return page.Locator("#audit-tab").Click() }, {Timeout: Float(8000)})`；断言 `resp.Status()==200`；`resp.JSON(&payload)` 解析 `events`；断言存在 `event.Action == "config_update"` 且 `event.Result == "success"`（`Object`/`Actor` 为 anonymous 回环，可能为空/`-`，**不**强断其值）。
  - DOM：`waitLoc("#audit-panel table tbody tr", Visible, 8000)`；`#audit-panel` `IsVisible()==true`（`switchStatsTab('audit')` 置 `display:block`）；`#audit-panel` `InnerText()` 含 `config_update`、含表头 `操作`、`主体`。反 false-green：断言 `#audit-panel` 的**表格行**（非空态）出现，且 `#stats-panel` `IsVisible()==false`（tab 切换真实生效）。

**DoD：** 云下载以 `submit-btn=preview` → `confirm-btn=提交` 的真实两段式闭环，断言了 body 的 url+filename、完成态与删除请求；取消用挂起源确定性触发；审计以 Go 侧真实审计事件 seed，断网络响应 + 表格行文本。lint 0。

---

### 任务 5：登录注册 + 存储配置 + 卷补漏

**文件：** 新增 `web/e2e/auth_config_e2e_test.go`
**复用：** `testServerCfg`、`testServer`、`pageFixture`、`waitLoc`、`requestJSON`、`seedUploadToVolume`
**配置依赖：** TOTP 注册 `testServerCfg(func(c *server.Config){ c.Registration.ForceTOTP = true })`；auth-bar 用默认；配置用默认；卷补漏用默认

- [ ] **步骤 1：TOTP 工具（`helpers_e2e_test.go`）**

```go
// totpCodeFromBase32 用 Go 侧 pkg/otp 对注册返回的 base32_secret（无 padding）算当前码。
// 服务端固定 30s 窗口：算码后须在同一进程内立即用于登录（避免跨 30s 边界过期）。
func totpCodeFromBase32(t *testing.T, secret string) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatalf("base32 解码 %q: %v", secret, err)
	}
	code, err := otp.NewTOTP(raw).Code(time.Now())
	if err != nil {
		t.Fatalf("TOTP 算码: %v", err)
	}
	return code
}
```

（import `encoding/base32`、`time`、`github.com/cocomhub/sproxy/pkg/otp`。）

- [ ] **TestAuth_RegisterTOTP_AndLogin** — UI 注册 + 登录 + 签名请求（**自包含，独立 server**）
  - server：`baseURL, _, cleanup := testServerCfg(t, func(c *server.Config){ c.Registration.ForceTOTP = true })`；`defer cleanup()`。
  - goto → 点 `#login-btn` → `waitLoc("#login-modal", Visible, 8000)` → 点 `#register-tab`（切注册面板）→ fill `#register-owner` = `"e2e-owner"`。
  - 控件+网络：`resp := ExpectResponse("**/api/credentials/register", click #do-register-btn, Timeout 8000)`；断言 `resp.Status()==200`；`resp.JSON(&reg)` 得 `{ak, owner, admin, otpauth_uri, base32_secret}`；断言 `ak` 以 `ak-` 开头、`base32_secret != ""`、`admin==true`（首位注册者）。
  - DOM：`waitLoc("#register-result", Visible, 8000)`；`#register-result` `InnerText()` 含 `AccessKey`、含 base32（或 `#register-result code` 计数 ≥2）；`page.Locator("#qr-register svg").Count() >= 1`（客户端 QR 渲染）。
  - 立即算码（同进程）：`code := totpCodeFromBase32(t, reg.Base32Secret)`。
  - 登录：点 `#login-tab` → fill `#login-ak` = `reg.AK`、`#login-code` = `code`。
  - 控件+网络（签名行证）：`req := ExpectRequest("**/api/files?*", click #do-login-btn, Timeout 10000)`；断言 `req.Headers()["authorization"]` 以 `SproxySig v=2 ` 开头（登录成功 → `applyWebLoginKeys` → `refreshList` 带签名）。
  - DOM/存储：`page.Evaluate("sessionStorage.getItem('sproxy_access_key')") == reg.AK`、`..._secret` 非空、`..._id` 非空；`#login-modal` `IsVisible()==false`（登录成功关闭）。
  - 自包含要点：本用例注册后 ring 非空 → 不得再断言任何匿名请求；所有后续交互经已建立的签名会话。

- [ ] **TestAuth_SaveKeysSigns** — auth-bar 保存 + 签名请求
  - server：默认 `testServer`（`ForceTOTP=false`）。
  - Go 侧注册：`http.Post(baseURL+"/api/credentials/register", "application/json", strings.NewReader(`{"owner":"bar-owner"}`))` → 解析 `{ak, sk}`（简单分支返回 `sk`/`skey_id`）；断言 200 且 `sk != ""`。
  - goto → 断言 `#file-list` 出现「请求失败」（此刻 ring 非空、页面无凭据 → 401；**可选**，若脆弱则跳过此断言）。
  - fill `#accessKey` = `ak`、`#token` = `sk`；点 `#save-access-btn`。
  - DOM：`sessionStorage sproxy_access_key/_secret/_id` 三键 == `ak`/`sk`/``（saveAccessKeys 写入；id 为空）。
  - 控件+网络：`req := ExpectRequest("**/api/files?*", click #refresh-btn, Timeout 10000)`；断言 `req.Headers()["authorization"]` 以 `SproxySig v=2 ` 开头；随后 `waitLoc("#file-table tr", Visible, 8000)` 或 `#file-list` 文本不再含「请求失败」。

- [ ] **TestConfig_UpdateMaxStorage** — 配置面板更新
  - server：默认 `testServer`。
  - goto → `#stats-btn` → `#config-tab` → `waitLoc("#cfg-max-storage", Visible, 8000)`（其触发 `GET /api/config` 已在此刻完成）。
  - fill `#cfg-max-storage` = `"104857600"`。
  - 控件+网络：`req := ExpectRequest("**/api/config", click #cfg-update-storage, Timeout 8000)`；断言 `req.Method()=="PUT"`；`requestJSON` 得 `{"max_storage_bytes":104857600}`。
  - DOM（更新后 `showConfig` 重拉）：轮询 `#cfg-max-storage` `InputValue()` 直到 `=="104857600"`（≤8s）；`#toast` 文本含 `配置已更新`（3s 内，`styles`）。

- [ ] **TestVolumes_SingleVolumeSelect** — 单卷补漏
  - server：默认 `testServer`（默认卷名 `"default"`）。
  - goto → `waitLoc("#upload-volume option[value='default']", Attached, 8000)`（`initUploadVolumeSelect` 的 `GET /api/volumes` 完成）。
  - 断言 options == `["", "default"]`（`Evaluate` 读 option value 数组，与 volumes 用例同法）。
  - `Evaluate("currentVolume()") == ""`（auto 初始）→ `SelectOption(Values:&[]string{"default"})` → 再 `Evaluate("currentVolume()") == "default"`（下拉 change → `setVolumeContext` 接线）。

**DoD：** 注册/登录用例自包含且以「Authorization 头 = SproxySig v=2」证明签名链路真实生效；配置用例以 PUT body + 重拉后 input value 双证；卷补漏证明单卷下 option 与 `currentVolume()` 接线。lint 0。

---

## 交付与验证命令

```bash
# 全部 e2e（Windows Git Bash；首次需装 chromium）
cd web/e2e
GOWORK=off go run github.com/mxschmitt/playwright-go/cmd/playwright install chromium
GOWORK=off go test -v -count=1 -timeout=600s ./...

# lint（必须 0 issues）
GOWORK=off golangci-lint run -c ../../.golangci.yml ./...

# 根 module lint / build 零回归
cd ../.. && make lint && make build-all
```

---

## 自检

1. **规格覆盖度（12 流程 → 任务映射）：**
   - 上传 → 任务 1 `TestFiles_Upload`；单删 → 任务 2 `TestFiles_Delete`；批量删 → 任务 2 `TestFiles_BatchDelete`；rename/批重命名 → 任务 2；mkdir/rmdir → 任务 1 `TestFiles_Mkdir` + 任务 2 `TestFiles_Rmdir`；面包屑 → 任务 1 `TestFiles_Breadcrumb`；分享 + `/s/{token}` → 任务 3；版本 → 任务 3；云下载 → 任务 4；审计 → 任务 4；登录/注册 → 任务 5；存储配置 → 任务 5；卷补漏 → 任务 5。**全覆盖**。
   - 明确不做：`/api/volumes/move`（无 UI）、`/api/storage/config`（无路由）、`#new-dir-name` 无 Enter 绑定（不为其编造路径）。
2. **占位符扫描：** 无「为上述写测试」「处理边界情况」类占位；每个场景给出具体控件、请求字段、DOM 断言与测试函数名。
3. **命令/签名一致性：** `testServer(t, opts ...bool)` 签名逐字保留；新增 `testServerCfg(t, mutate)`；所有 Playwright 调用使用本 module 实际存在的 API（`OnDialog`/`Locator.WaitFor`/`ExpectRequest`/`ExpectResponse`/`PostDataBuffer`/`Headers`）；路径/query/字段名与 `web/static/**` 及 `pkg/server/**` 逐条核对（`force=true`、`version_id`、`max_storage_bytes`、`{url,filename}`、`{files:[...]}`、`{operations:[...]}`、`X-File-Checksum`、`subdir`）。
4. **风险与处置：**
   - **`ui-e2e` 超时**：新增 ~25 个 headless Chromium 用例 → 已含抬 `-timeout=600s` 步骤；CI 若仍偏慢，拆 `-run` 分组或按文件分 package（本计划暂不拆）。
   - **`testServer` 重构回归**：`testServerCfg` 启动体必须与旧体逐字等价，任务 0 步骤 7 的冒烟为门禁。
   - **版本「两次真上传产版本」**：依赖 `upload_handler.go:342` 覆盖写产版本；若首版不产版本，用例会失败——**视为真实缺陷**（不得改成直写 `testFile` 绕过，那会被 inventory 明令禁止且回到 `.empty-msg` 假绿）。
   - **云下载回环源**：必须 `CloudDownloadAllowPrivate=true`（默认 false）；漏设将 403/失败，属配置错误而非 flake。
   - **dialog 序列竞态**：`respondDialogs` 的分派闭包若跨 goroutine 计数需加锁；优先按 `d.Message()` 无状态分派。
   - **lint CI 归属**：本 PR 只保证本地 0 issues；CI 门禁随 PR-F1 的 `web/e2e` lint 接线生效（当前 master 的 `lint` job 扫不到嵌套 module）。
   - **审计 actor/object 为空**：anonymous 回环无 AK → `RecordAudit` 的 Actor 为空、`config_update` 无 Object；用例不断言其值，只断 action/result/DOM 文本。

## 执行交接

计划落地为单一 PR：新增 `web/e2e/{helpers,files,share_version,cloud_audit,auth_config}_e2e_test.go` + 修改 `ui_e2e_test.go` 与 `.github/workflows/ci.yml`。执行方式：**子代理驱动（SDD）**——每任务全新 implementer + 独立 reviewer；任务内遵守红线（真交互/非存在性/dialog 先装/轮询 ≥10s/lint 0），全部用例本地跑绿且 `web/e2e` lint 0 issues 后合并。若 PR-F1 已落地，则 web/e2e 的 lint 与 test 由 F1 的 CI 接线承担门禁；否则合并前须在 CI 中补一条 `cd web/e2e && GOWORK=off golangci-lint run -c ../../.golangci.yml ./...`（随 F1 CI 接线生效）。
