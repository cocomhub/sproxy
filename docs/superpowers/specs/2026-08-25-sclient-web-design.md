---
title: sclient 前端库（Web SDK）—— 领域化隧道传输与跨端对齐
status: approved
---

# sclient 前端库设计（`web/static/sclient/`）

> 日期：2026-08-25
> 分支：feature/mesh-tunnel
> 状态：**approved**（用户已确认分层、命名、领域接口、原生多文件加载、类型声明、签名入库、config/log）

## 1. 背景与动机

当前分支认证重构后，Web 端 `tunnel.js` 依赖废除的 `tunnel_key`，页面直连分支又因
`app.js headers()` 里 `if (tunnelHexKey) return h` 直接跳过 SproxySig 而 401 —— **Web 端
处于"隧道死代码 + 直连断链"状态**。同时 UI 代码 20+ 处散落 `if (tunnelHexKey) tunnelRequest(...)
else fetch(...)` 分支，维护爆炸，且无法统一接受"服务端控制是否走隧道"的新开关。

目标：**一个与 Go `pkg/client`（FileClient）对称的前端领域库 `sclient`**，UI 只调用领域方法、
绝不拼 path/选传输/碰签名；库自身完成认证、传输、加解密、配置与日志；支持"服务端 web.tunnel
开关下发 + 本地调试 override"双开关；明文 HTTP 部署下 Web 读写全量走加密隧道（选 A）。

## 2. 已确认决策点

| # | 决策 | 说明 |
|---|------|------|
| 1 | SDK 命名 `sclient` | 与 CLI 端品牌一致，后续可作 npm 开源包 `sclient-js` 的基础 |
| 2 | 领域命名空间分组 | `sclient.files.*` / `sclient.cloud.*` / `sclient.share.*` / `sclient.config.*` / `sclient.hub.*` |
| 3 | 独立包结构（多文件） | `web/static/sclient/` 自包含目录：crypto / sig / transport / api 子目录 / config / log / index |
| 4 | 原生多文件加载 | UI 用 `<script>` 标签按序加载，不用打包器；`index.js` 挂全局 `sclient`（UMD 风格） |
| 5 | 类型声明 | `index.d.ts` 完整导出领域类型；UI 非 TS 时不影响运行 |
| 6 | 签名入库 | SproxySig 签名在 `sclient/sig.js`；UI 永不碰签名与 AK 派生 |
| 7 | 严格领域方法 | 不开放 `request()`；新增接口交互必须通过领域 SDK |
| 8 | 传输 | `transport='auto'`（跟随服务端 `web.tunnel` 开关）⊕ localStorage 调试 override |
| 9 | 写操作也走隧道 | 全量（选 A），分块上传每块加密 |
| 10 | 凭据 | `createSclient` 注入 AK/SK；SK 存 sessionStorage（保持现状） |
| 11 | 模式默认 | `web.tunnel: true`（默认走隧道） |
| 12 | 页面调试开关持久化 | localStorage 记忆（非敏感开关） |

## 3. 目录结构与加载

```
web/static/sclient/
  index.js        # 入口：createSclient 工厂 + 全局 sclient 命名空间（UMD 挂全局）
  transport.js    # 传输核心：effectiveMode / directFetch / tunnelRun / 帧解析
  crypto.js       # WebCrypto 封装：bytes/hex、sha256、HMAC-SHA256、HKDF、AES-256-GCM
  sig.js          # SproxySig 签名头构造（含 UNSIGNED 语义透传）
  config.js       # 默认配置 + 覆盖（服务端 web.tunnel 开关 / 本地 override / 阈值 / log）
  log.js          # 简易可替换 logger（默认 console，level: debug|info|warn|error）
  api/index.js    # 组装各领域命名空间导出一处
  api/files.js    # 领域：文件 CRUD + 分块/简单上传 + 下载流
  api/cloud.js   # 领域：云端下载（任务/组/归档）
  api/share.js    # 领域：分享（创建/列表/撤销）
  api/config.js   # 领域：配置读取/更新（含 web.tunnel 开关下发）
  api/hub.js      # 领域：中继账本（nodes/stats/remove）
  index.d.ts      # TypeScript 声明（type-only，兼容纯 JS 使用）
```

`index.html` script 顺序：`sha256.js`（暂保留，crypto 已含实现后可去）→ `sclient/index.js`
→ `upload.js` → `app.js`。

## 4. 接口设计

### 4.1 创建与配置

```js
import { createSclient, SclientError } from './sclient/index.js'; // 或全局 sclient.createSclient

const sc = createSclient({
  baseUrl: '',                  // 默认同源
  accessKey: 'sk-...-<hex>', accessKeySecret: '<64hex>',
  transport: 'auto',           // 'tunnel' | 'direct' | 'auto'
  log: { level: 'warn' },
});
```

### 4.2 领域命名空间（方法全部 promise）

| 命名空间 | 方法 | 返回 |
|----------|------|------|
| `sc.files` | `list(subdir)` | `{files}` |
| | `search(q, subdir)` | `{files}` |
| | `stat(name)` | 单文件元信息 |
| | `upload(file, {subdir, onProgress, forceChunked})` | `{success, message}` |
| | `download(name, {onProgress})` | Blob |
| | `delete(name, checksum)` / `rename(from,to,checksum)` / `mkdir(dir)` / `rmdir(dir)` | `{success,message}` |
| | `batchDelete(files)` / `batchRename(pairs)` | `{success,message}` |
| | `archive(entries)` / `archiveDirs()` | `{success,message}` / 目录列表 |
| | `versions(name)` / `restoreVersion(name,id)` / `deleteVersion(name,id)` | |
| `sc.cloud` | `download(urls)` / `batch(urls, filenames)` / `list()` / `task(id)` | |
| | `cancel(id)` / `remove(id)` / `archive(taskId)` | |
| | `groups.*`（create/list/get/cancel/delete/resume/archive） | |
| `sc.share` | `create({filename, password?, expire_in?})` → token | |
| | `list()` / `revoke(token)` | |
| `sc.config` | `get()` / `update(patch)`（含 `web_tunnel` 下发） | |
| `sc.hub` | `nodes()` / `stats()` / `removeNode(id)` | |
| `sc.diagnostics` | mode/凭据状态/阈值 | |

### 4.3 底层核心（仅 SDK 内部）

- `transport.coreRequest(method, path, {bodyBytes, headers, download})`：解析 effectiveMode → 隧道/直连 → 归一 `{status, headers, body(ArrayBuffer|JSON)}`。领域方法统一调它。
- `sig.signHeader(method, path, bodyBytes)`：SproxySig 头构造，加密密文/无 body 语义处理。
- `crypto.deriveTunnelKey(sk, mesh)`：= HKDF(SK, salt='sproxy-tunnel-key-v1', info=mesh) 32B，与 Go `tunnel.DeriveTunnelKey` 一致（fixture 锁死）。
- `crypto.tunnelFrame` 构造/解析帧 `[4B len + AES(meta/body)]`。

## 5. 数据流（mode='tunnel'）

UI → 领域方法 → `coreRequest`：
1. 有效模式 = `serverWebTunnel` 与本地 override 交
2. 派生密钥（缓存，凭据变更才重置）
3. 构造 metadata 帧 `[4B len + AES(metaJSON)]`，meta 含 method/url/headers
4. `/tunnel` 请求签名：`sig.signHeader('POST', '/tunnel', metaFrame)`（body_sha256=帧哈希）
5. fetch 发送 → 读取帧流 → 解密 metadata + body
6. 返回 `{status, headers, body}`

## 6. 错误处理（`SclientError`）

| code | 场景 | UI 提示 |
|------|------|--------|
| `E_AUTH` | 401/403、AK/SK 无效、派生失败 | 重新保存凭据 |
| `E_DECRYPT` | 响应解密失败 | 隧道响应解密失败 |
| `E_NETWORK` | 网络错误 | 网络错误 |
| `E_SERVER`（已实现） | 5xx：隧道/直连请求响应 !ok（transport.js：tunnelRun/directRun）| 服务器错误（HTTP status） |
| `E_INTERNAL`（已实现） | 内部守卫 / 参数错误（如 coreRequest 非法参数守卫、领域层参数校验） | 客户端内部错误 |
| 业务 4xx | `{success:false, message}` 不 throw | 服务器返回 message |
| 5xx | throw `{code:'E_SERVER', status}` | 服务器错误 |

## 7. 测试计划

- `web/static/sclient/*.test.js`（vitest/node test）覆盖：领域方法→path/method 映射、HKDF 与 Go 已知向量、签名头与 Go 已知向量、隧道/直连 mock fetch 全链路。
- `make web-test` 加入 sclient 用例（或新增 target）。
- E2E 浏览器后续再做；单元层先保住。

## 8. 服务端配套

| 文件 | 改动 |
|------|------|
| `pkg/server/config.go` | `WebConfig{Tunnel bool}`；默认 `true`；加入 config
| `pkg/server/config_api.go` | `configResponse` 加 `web_tunnel`；`updateConfigRequest` 加 `web_tunnel`
| `config.example.yaml` | 补 `web.tunnel: true` 段注释
| `pkg/server/config_test.go` / `config_api_test.go` / `integration_test.go` | 断言 `web_tunnel`

## 9. 范围边界（YAGNI 明确不做）

- 不开放 `request()` 通用兜底
- 不接 hub 注册 / mesh 连接（那是 CLI/mesh node 的领域）
- 不做轮询/重试框架（沿用 upload 级重试）
- 不做 npm 发布（结构兼容，实际发布后续定）
- 不做 PWA / Service Worker

## 10. 风险与兼容

- **HKDF 一致性**：Web 派生必须与 Go `tunnel.DeriveTunnelKey` 完全一致（salt/info / 长度 / mesh 提取 `AccessKeyMesh` 语义）—— fixture 锁死，Go 与 JS 各持一份同 fixture。
- **签名 canonical 一致性**：JS `sproxysig` canonical 拼接与 Go 完全一致（含 EscapedPath/RawQuery 语义近似——JS 用 encodeURI 不能完全等价，用 URL API 提取 path+query 对齐）。
- 旧 `tunnel.js` 删除其内 localStorage 'sproxy_tunnel_key' 读取；`app.js/index.html` 清理 `tunnelHexKey`/`saveTunnelKey`/tunnel-key 输入框。

## 11. 完成定义（DoD）

- [ ] `web/static/sclient/**` 全文件就位，`index.html` 按序加载
- [ ] `app.js/upload.js` 移除全部 `tunnelHexKey` 分支，改用 `sclient.*` 领域调用
- [ ] `make web-test` 全绿（含新增 sclient 用例）
- [ ] `golangci-lint run ./pkg/... ./cmd/...`（主仓 + 子 module）0 issues
- [ ] `go test ./pkg/server/... ./cmd/server/...` 通过；E2E `go test ./test/...` 通过
- [ ] 真实浏览器手测：明文 HTTP 下 list/download 内容不可见、凭据无效时报 E_AUTH

---