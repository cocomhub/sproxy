# task-sha256-sdk-report

日期：2026-08-26 分支：`feature/mesh-tunnel` 基线 `d971f8a`

## 任务

重构 sproxy Web 的 sha256 依赖结构，让 sclient SDK 完全自包含（不引用目录外文件）。

## 改动（提交 `refactor(web)…`）

1. `web/static/static/sha256.js` → `web/static/sclient/sha256.js`（`git mv`），UMD 浏览器暴露名由裸 `root.Sha256` 改为 `root.sclientSha256`（Node 侧 `module.exports` 不变）。
2. `web/static/sclient/api/files.js`：大文件流式路径 `new globalThis.Sha256()` → `new sclientSha256()`；注释同步。
3. `web/static/app.js`：`computeFileSHA256` 用 `new sclientSha256()`；顶部注释「依赖 sha256.js」→「依赖 sclient/sha256.js」；函数注释同步。
4. `web/static/upload.js`：顶部依赖注释同步。
5. `web/static/index.html`：外层 `<script src="sha256.js">` 删除，改为在 `sclient/crypto.js` 之前加载 `<script src="sclient/sha256.js">`（保证 files.js 之前）。
6. `web/static/sclient/sclient.test.js`：`require('../sha256.js')` → `require('./sha256.js')`；注入全局由 `globalThis.Sha256` 改为 `globalThis.sclientSha256`；相关注释同步。
7. `web/static/sha256.js` 已 git rm（随 git mv 完成）。

## 引用面盘点（全仓库确认）

- 无 `<script src="sha256.js">`、`require('../sha256.js')`、`self.Sha256`、`globalThis.Sha256` 活跃残留。
- 仅剩的命中均为历史文档 / 报告 / sclient/sha256.js 自身头注释（均已确认非活跃代码引用）。
- `docs/superpowers/specs`（设计稿）、`docs/superpowers/plans`（历史 plan、含 `mark-unfixed` 列表标记）只描述旧路径，属历史文档不更新。
- 大型流式哈希测试注入全局改为 `globalThis.sclientSha256`，与浏览器端 files.js 依赖一致。

## 测试结果

`cd web/static && node --test sclient/sclient.test.js`：tests 42, pass 42, fail 0（含 `sha256.js 增量实现` 与 `computeSHA256 大文件流式分片` 回归用例）。

`node --check` 全部通过：sclient/sha256.js、sclient/api/files.js、app.js、upload.js、sclient.test.js。

golangci-lint 无关（无 Go 改动）。
