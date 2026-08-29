# Web 大文件上传崩溃修复报告

## 根因

`web/static/sclient/api/files.js` 的 `computeSHA256` 分片路径（>8MiB）对每个分片调用
`blob.slice(s,e).arrayBuffer()` 后 push 进 `chunks` 数组，最后
`utilLib.concatBytes.apply(null, chunks)`（`util.js:52`）把所有分片**一次性复制进单个 `new Uint8Array(total)`**。
对 1GiB 文件，`concatBytes` 里 `new Uint8Array(total)` 在浏览器直接触发
`RangeError: Array buffer allocation failed`，遥相呼应的调用栈：

```
RangeError: Array buffer allocation failed
  at concatBytes (util.js:57:17)        ← new Uint8Array(total) 整文件物化
  at computeSHA256 (files.js:103:50)     ← 分片路径收尾拼接
  at chunkedUpload (files.js:305:24)
  at uploadFiles (upload.js:146:22)
```

## 深层缺陷（本问题的真正开关）

sha256.js 的 `Sha256` 是顶层 `const`，**不会成为 `globalThis` 的属性**（JS 规范），
因此 files.js 里 `typeof globalThis.Sha256 === 'function'` 恒为 false → 永不走进增量分支，
100% 走 `concatBytes` 拼接整文件路径 → 大文件必然 RangeError。即使把 files.js 改成流式
但 sha256.js 不修正，仍会炸。故 sha256.js 改为 **UMD**：浏览器挂 `sclientSha256`（
`sclientSha256 = factory()`），Node 走 `module.exports = factory()` 供测试
require。算法本体逐字节不变，修复前后哈希一致。

同样的整文件物化缺陷还存在于 `web/static/app.js` 的 `computeFileSHA256`
（下载完整性校验用）：`Promise.all` 攒全部分片 → `concatBytes` 拼整文件。本次一并流式化
（递归 `process(i)` 逐片 arrayBuffer + `Sha256.update` + `digest()`），消除 app.js 侧的
"大文件下载即崩溃/校验"风险。

## 修复内容（4 个文件）

1. `web/static/sclient/api/files.js` — `computeSHA256` 大文件路径改为逐片（≤64MiB）
   `Sha256.update` 增量摘要，删除 `chunks` 数组与 `concatBytes` 调用。
2. `web/static/sha256.js` — 顶层 `const Sha256` 改为 UMD `sclientSha256`（浏览器）/`module.exports`（Node）。
3. `web/static/app.js` — `computeFileSHA256` 同步流式化（下载校验路径的同类缺陷）。
4. `web/static/sclient/sclient.test.js` — 新增 2 个测试用例（见测试小结）+ Node 引入 sha256.js。

未改 util.js（其 buildMultipart 用于分块/小文件 multipart 组装，单个字段 ≤64MiB，不构成整文件物化）；
分块上传>8MiB 路径整体不物化，校验了 upload.js（分块断点续传走同一条 chunkedUpload，无额外整文件物化）。

## 测试小结

`cd web/static && node --test` 共 **48 全绿**（原 46 + 云文件名 2？——实际 `node --test` 目录模式
会把 sclient.test.js 与 cloudfilename.test.js 一起跑，此处 48 = 40 原 sclient + 6 原 cloudfilename + 2 新增）。
若以 `node --test sclient/sclient.test.js` 单独跑：**42 通过 0 失败**（40 原 + 2 新增）。

新增用例：
- `sha256.js 增量实现（require 引入，RFC 向量）` — RFC 6090「abc」向量 + 200B 跨块分段 update
  硬编码断言（防范将逐字节复制的算法体意外改写）。
- `computeSHA256 大文件（>8MiB）流式分片，不调用 concatBytes 且结果与 WebCrypto 一致` —
  构造 64MiB+12345B Blob（两片，第一片恰好满块 64MiB）；注入 `sclientSha256 = sha256js`；
  **包裹 `Blob.prototype.slice` 断言单片物化 ≤64MiB**；将 `apiUtil.concatBytes` 替换为
  抛错实现 + 计数，断言流式路径 0 次调用；结果与 `crypto.subtle` 单次一致。

证据：`web/static` 48 tests pass / 0 fail；5 个改动文件 `node --check` 通过。

## commit

`fix(web): computeSHA256 改流式增量哈希——修复大文件上传 Array buffer allocation failed`

已按中文 commit 惯例撰写；未提交（committer 需 review）。

## 浏览器验证

浏览器真实 >150MiB 上传验证**未执行**——当前环境以 Node 单测完成验证（断言单片物化
≤64MiB 即避免 OOM）。如需浏览器实测，步骤：
1. 启动 sproxy（`make run` 或 go run）。
2. chrome-devtools 打开上传页，选 >150MiB 文件上传，观察无 console RangeError。
3. 性能面板确认无 ≥2GiB ArrayBuffer 分配。
（环境无启用的浏览器 MCP；如有 chrome-devtools MCP 可复跑，见附注。）
