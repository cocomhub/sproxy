# sclient 前端库（Web SDK）实现计划

> **面向 AI 代理的工作者：** 必需子技能：subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法追踪进度。

**目标：** 构建 `web/static/sclient/` 领域化前端库（对标 Go `pkg/client` 的 FileClient），提供 `files/cloud/share/config/hub` 领域命名空间 + 由 SK 派生隧道密钥 + 服务端 `web.tunnel` 开关控制页面走隧道/直连 + 类型声明 + config/log，并把 `app.js`/`upload.js` 全部 API 调用迁移到领域方法（UI 不再碰 path/签名/传输）。

**架构：** UI（app.js/upload.js）→ `sclient` 领域 API → `transport.coreRequest(method, path, opts)`（内部按 `effectiveMode()` 决定走加密隧道 `POST /tunnel` 或直连 `fetch`+SproxySig）→ 服务端。隧道密钥由 `crypto.deriveTunnelKey(SK, mesh)`（HKDF-SHA256，salt=`sproxy-tunnel-key-v1`，info=mesh，32B）派生，两端参数与 Go `tunnel.DeriveTunnelKey` 完全一致。服务端新增 `web.tunnel`（默认 true）随 `/api/config` 下发，页面据 localStorage debug override 决定最终模式。

**技术栈：** 纯前端 JS（无构建器，原生 `<script>` 按序加载）+ WebCrypto（`crypto.subtle`：`digest`/`sign` HMAC/`deriveBits` HKDF / `encrypt` AES-GCM）+ Go（`web.tunnel` 配置字段、`/api/config` Response 加字段）+ Node 单测（vitest 或直接 Node `node:test`——**与现有 `cloudfilename.test.js` 的 `make web-test` 对齐，用 Node 原生 `node:test` 判定**）。

属 spec：`docs/superpowers/specs/2026-08-25-sclient-web-design.md`。

---

## 文件结构

| 动作 | 文件 | 职责 |
|------|------|------|
| 新建 | `web/static/sclient/index.js` | `createSclient` 工厂 + 全局 `sclient` 命名空间（UMD 风格）；组织模块 |
| 新建 | `web/static/sclient/config.js` | 默认配置 + override（`transport`、`localStorage` 调试开关、阈值） |
| 新建 | `web/static/sclient/log.js` | 简易可替换 logger（level: debug/info/warn/error） |
| 新建 | `web/static/sclient/crypto.js` | bytes/hex、sha256、HMAC-SHA256、**HKDF-SHA256 派生 `deriveTunnelKey`**、AES-256-GCM 帧加解密 |
| 新建 | `web/static/sclient/sig.js` | SproxySig 签名头构造（canonical 与 Go 对齐） + `UNSIGNED` 语义 |
| 新建 | `web/static/sclient/transport.js` | `coreRequest`：`effectiveMode()` / `directFetch` / `tunnelRun` / 帧解析 |
| 新建 | `web/static/sclient/api/files.js` | 文件 CRUD + 简单/分块上传 + 下载 Blob + 批量 + archive/versions |
| 新建 | `web/static/sclient/api/cloud.js` | 云端下载：任务/组/归档 |
| 新建 | `web/static/sclient/api/share.js` | 分享 create/list/revoke |
| 新建 | `web/static/sclient/api/config.js` | 配置 get/update（含 `web.tunnel` 下发） |
| 新建 | `web/static/sclient/api/hub.js` | hub nodes/stats/removeNode |
| 新建 | `web/static/sclient/api/index.js` | 组装各领域命名空间 |
| 新建 | `web/static/sclient/index.d.ts` | TS 类型声明（领域类型 + createSclient opts/返回） |
| 新建 | `web/static/sclient/sclient.test.js` | Node 单测（`node:test`）：领域方法→path/method 映射、HKDF 与 Go 向量一致、SproxySig canonical 与 Go 向量一致、隧道/直连 mock fetch 全链路 |
| 修改 | `web/static/index.html` | script 顺序：sha256 → sclient/index.js → upload.js → app.js；删 Tunnel Key 输入框；加调试开关 UI |
| 修改 | `web/static/app.js` | 顶部 `const sc = createSclient(...)`；20+ `tunnelHexKey`/`fetch` 分支全换 `sc.*` 领域调用；删 `saveTunnelKey`、`headers()`、`sproxysigAuthHeader`、`tunnel.js` 依赖项 |
| 修改 | `web/static/upload.js` | 上传函数接受 `transport` 由 SDK 决定；内部调用 `sc.files.upload` |
| 修改 | `web/static/tunnel.js` | 删除（功能并入 sclient/transport.js） |
| 修改 | `pkg/server/config.go` | 加 `WebConfig{Tunnel bool}` 默认 true；Config 加 `Web WebConfig` |
| 修改 | `pkg/server/config_api.go` | `configResponse` 加 `web_tunnel`；`updateConfigRequest` 加 `web_tunnel` |
| 修改 | `config.example.yaml` | 补 `web.tunnel` 段 |
| 修改 | `pkg/server/config_test.go` / `config_api_test.go` | 断言 `web_tunnel` 默认 true 与下发 |
| 新建 | `web/static/sclient/fixtures.js` | 与 Go 共享的 HKDF/签名测试向量（**TDD**：JS 侧先引，Go 侧后续对照；本计划先把 JS fixture 写死与 Go 值核对过） |
| 修改 | `Makefile` | `web-test` target 加入 sclient 测试（或新增 `web-test-sclient`） |

---

## 任务清单

### 任务 1：服务端 `web.tunnel` 配置字段 + `/api/config` 下发（Go，TDD）

**文件：**
- 修改：`pkg/server/config.go`
- 修改：`pkg/server/config_api.go`
- 修改：`config.example.yaml`
- 测试：`pkg/server/config_test.go`、`pkg/server/config_api_test.go`

- [ ] **步骤 1：编写失败的 Go 测试**（断言 `web.tunnel` 默认 true + 下发）

```go
// config_test.go 内新增（若已存在等价断言可复用）
func TestConfig_WebTunnelDefault(t *testing.T) {
	c := Default()
	if !c.Web.Tunnel {
		t.Fatal("web.tunnel 默认应为 true")
	}
}
```

```go
// config_api_test.go 内新增：GET /api/config 响应含 web_tunnel: true
// 参考现有 configHandler 测试写法（SetResponseLogger + httptest）
```

- [ ] **步骤 2：运行确认失败**（`c.Web` undefined / 响应缺字段）
```bash
cd /d/workdir/leon/cocomhub/sproxy && go test -count=1 ./pkg/server/... 
```

- [ ] **步骤 3：实现最小代码**

`pkg/server/config.go` 在 `Config` 结构体加 `Web WebConfig \`yaml:"web" mapstructure:"web"\``，定义：

```go
// WebConfig 控制 Web UI 的传输行为。
// Tunnel=true 时 Web 领域方法默认走加密隧道（由 SK 派生密钥）；false 走直连 SproxySig。
// 页面另有 localStorage 调试开关，可临时覆盖（仅调试用，非敏感开关可持久化）。
type WebConfig struct {
	Tunnel bool `yaml:"tunnel" mapstructure:"tunnel"`
}
```

`Default()` 中 `Web: WebConfig{Tunnel: true}`。

`config_api.go` 的 `configResponse` 加 `WebTunnel bool \`json:"web_tunnel"\``，`configHandler` 填充 `cfg.Web.Tunnel`；`updateConfigRequest` 加 `WebTunnel *bool \`json:"web_tunnel,omitempty"\`` 并在 `updateConfigHandler` 应用（非 nil 才覆盖）。

- [ ] **步骤 4：运行确认通过 + lint**
```bash
go test -count=1 ./pkg/server/... && golangci-lint run ./pkg/server/... 
expect: PASS, lint 0 issues
```

- [ ] **步骤 5：config.example.yaml 补段 + commit**

```bash
# web 段注释（接入点见文件 storage 段附近）
git add pkg/server/config.go pkg/server/config_api.go pkg/server/config_test.go pkg/server/config_api_test.go config.example.yaml
make fmt  # addlicense + gofmts
# 注：pre-commit 钩子会自动跑 make build；先手跑 go test + lint 确认再 commit

git commit -m "feat(server): web.tunnel 配置字段 + /api/config 下发（默认 true）"
```

### 任务 2：`sclient` 基础设施（crypto / log / config）

**文件：**
- 新建：`web/static/sclient/crypto.js`、`web/static/sclient/log.js`、`web/static/sclient/config.js`
- 测试：`web/static/sclient/sclient.test.js`

- [ ] **步骤 1：编写失败测试——HKDF 派生与 Go 已知向量一致、SHA-256/HMAC 基础正确**

```js
// sclient.test.js（用 node:test + assert/strict，不用第三方断言）
const { createSclient } = require('./index.js');
// 用于底层验证：从全局挂在 module 上的内部（测试面向公开 API，但底层函数可导出用于验证）

// Go fixture：DeriveTunnelKey("<sk>", "<mesh>") 前 8 字节 hex。
// 先用 Go 生成（任务 2 步骤 3 后端给源码里的值），此处写死。
const FIXTURES = [
  { sk: '2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5',
    mesh: 'meshA', keyHex: 'PLACEHOLDER_ACTUAL_VALUE' },
  { sk: '2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5',
    mesh: '', keyHex: 'PLACEHOLDER_ACTUAL_VALUE' },
];
```

- [ ] **步骤 2：运行确认失败**（模块不存在，`createSclient` undefined）
```bash
cd /d/workdir/leon/cocomhub/sproxy/web/static
node --test sclient.test.js   # 或 node --test sclient/
```

- [ ] **步骤 3：实现 `crypto.js`/HKDF**（WebCrypto 无现成 HMAC-扩展 HKDF？有：`crypto.subtle.deriveBits({name:'HKDF', hash:'SHA-256', salt, info}, key, 256)` 需先 `importKey('raw', secret, 'HKDF')`）

```js
async function deriveTunnelKey(secretHex, mesh) {
  const secret = hexToBytes(secretHex);          // 32B
  const ikm = await crypto.subtle.importKey('raw', secret, 'HKDF', false, ['deriveBits']);
  const bits = await crypto.subtle.deriveBits(
    { name: 'HKDF', hash: 'SHA-256', salt: new TextEncoder().encode('sproxy-tunnel-key-v1'), info: new TextEncoder().encode(mesh) },
    ikm, 256);
  return new Uint8Array(bits);
}
```
（`hexToBytes`/`bytesToHex`/`sha256Hex`/`hmacSHA256Hex`/`aesGcmEncrypt` 基础工具同文件）

及 `log.js`（范围内 logger）、`config.js`（默认配置 + override）。

- [ ] **步骤 4：验证通过 + commit**

（跑 node test，对照 fixture 核 HMAC/AES 一致性；New field 'keyHex' 先用 Go 侧生成的真实字节）

### 任务 3：`sclient` SproxySig 签名（sig.js）—— 对齐 Go canonical 拼接

**文件：**
- 新建：`web/static/sclient/sig.js`
- 测试：`web/static/sclient/sclient.test.js`（追加用例）

- [ ] **步骤 1：编写失败测试**（canonical 拼接向量对照 Go）
```js
// Go fixture: Sign(sk, Header{ak,ts,exp,nonce,body_sha256='UNSIGNED'}, 'POST', '/tunnel', '') → sig hex
// 从 Go 侧拿真实值（用现有 sproxysig_test.go 或新 Go 测试临时导出），写死到 fixture。
```

- [ ] **步骤 2：确认失败、实现 sig.js**（`signHeader(method, path, bodyBytes, {unsigned})` 内部拼 canonical + HMAC）
- [ ] **步骤 3：验证 + commit**

### 任务 4：传输核心 `transport.js`（隧道帧 + 直连签名 + effectiveMode）

**文件：**
- 新建：`web/static/sclient/transport.js`
- 测试：`web/static/sclient/sclient.test.js`（追加）

- [ ] **步骤 1：写失败测试**——mock fetch：
  - 隧道模式：mock fetch 返回帧（meta+body 用已知派生密钥加密）→ 断言解密 body 正确
  - 直连模式：断言加签名头
  - `effectiveMode()` 服务端开 + local override 三态
- [ ] **步骤 2：实现 transport.js**（`coreRequest`，把现有 `tunnel.js` 的帧构造/解析搬入，改密钥来源为 `deriveTunnelKey`）

```js
async function coreRequest(method, path, { headers, bodyBytes, download } = {}) {
  const mode = effectiveMode();
  if (mode === 'tunnel') {
    const key = await deriveTunnelKey(credentials.secret, accessKeyMesh(credentials.key));
    // 现有隧道帧逻辑 + POST /tunnel + 响应解密
  }
  // direct: fetch + sig header
}
```
（`accessKeyMesh` 语义与 Go `AccessKeyMesh`：`sk-<mesh>-<16hex>` → mesh；`sk-<hex>` → ''）

- [ ] **步骤 3：验证 + commit**

### 任务 5：领域 API 封装（files/cloud/share/config/hub）

**文件：**
- 新建：`web/static/sclient/api/index.js`、`api/files.js`、`api/cloud.js`、`api/share.js`、`api/config.js`、`api/hub.js`
- 测试：`web/static/sclient/sclient.test.js`（追加）

- [ ] **步骤 1：写失败测试**——领域方法→path/method 映射（对照 app.js 现有调用推导的映射表，列到 fixture）：
```js
// 例如 files.list('') → coreRequest('GET', '/api/files?subdir=') ；files.download('x') → coreRequest('GET', '/download?filename=x', {download:true})
```
- [ ] **步骤 2：实现领域方法**（每个方法内部调 `coreRequest`，处理 JSON 编解码 / checksum / multipart——**multipart 构建从 upload.js `buildMultipartBody` 搬入 sclient（或保留在 upload.js 传入 bodyBytes）；本任务它搬入 sclient/crypto？不——属于传输层域，放 `sclient/api/common.js`？最小化：放 sclient/util.js**）
- [ ] **步骤 3：验证 + commit**

### 任务 6：UI 迁移 app.js / upload.js + index.html script/开关 UI

**文件：**
- 修改：`web/static/app.js`、`web/static/upload.js`、`web/static/index.html`
- 删除：`web/static/tunnel.js`

- [ ] **步骤 1：app.js 顶部接入**
```js
const sc = createSclient({
  accessKey, accessKeySecret,
  transport: 'auto',   // 服务端 web.tunnel 控制
});
// 保存按钮仍写 sessionStorage；加 debug 开关（checkbox）读写 localStorage 'sproxy_web_transport_override'
```
- [ ] **步骤 2：替换 20+ 处 tunnelHexKey/fetch 分支 → sc.* 领域调用**（列表见上）
- [ ] **步骤 3：删除 headers()/sproxysigAuthHeader()/saveTunnelKey/tunnel-key 输入框**，upload.js 改为调用 sc.files.upload（内部自己看 transport）
- [ ] **步骤 4：index.html 删 tunnel-key 输入 + 加 '走隧道' 调试 checkbox**
- [ ] **步骤 5：`make web-test` 全绿 + `go build` 通过（web 不是 Go 包但 Makefile build 会 copy static）**
- [ ] **步骤 6：lint + commit**

### 任务 7：测试补齐 + 全量验证 + 文档

**文件：**
- 测试：`web/e2e`（若存在），否则跳过
- 修改：`Makefile`（web-test 含 sclient）
- 修改：`README.md` / `docs/cli.md`（可选——web UI 隧道说明）

- [ ] **步骤 1：`make web-test`**（确认 sclient.test.js 已在 target 内，Node 原生 `node --test` 可用）
- [ ] **步骤 2：`make test-all` + `make lint` 0 issues**（全子 module）
- [ ] **步骤 3：浏览器/curl 手测核验**（明文 HTTP 下 list/download 均经隧道，页面 DevTools Network 只见 POST /tunnel；关掉隧道验证直连恢复）
- [ ] **步骤 4：确认无残留**：`grep -rn "tunnelHexKey\|sproxy_tunnel_key\|saveTunnelKey\|tunnel-key" web/` 应无命中（保留 sclient 内部无引用）
- [ ] **步骤 5：README/docs 更新 + commit**

---

## 自检

- **规格覆盖**：1-2↔决策 1-4,7（命名/分组/多文件/类型/签名）；任务 6↔决策 8-12（transport/全量/凭据/默认/override）；服务端`web.tunnel`配套↔spec 第 8 节；测试/DoD↔第 7、11 节。无遗漏。
- **占位符扫描**：`FIXTURES` 两处 `PLACEHOLDER_ACTUAL_VALUE` 是**实现时必须用 Go 侧真实值填的 TDD 向量**，非懒占位——任务 2/3 步骤里明确去 Go 侧生成真实值。
- **类型一致性**：`deriveTunnelKey`、`coreRequest`、`createSclient`、`sc.files.*` 跨任务一致；领域方法清单与 spec 4.2 严格一致。
- **顺序依赖**：Go 任务 1 先（提供 `web.tunnel` 下发），JS 任务 2-6 随后（依赖密钥派生/传输/领域），任务 7 收尾。每个任务独立可测。
