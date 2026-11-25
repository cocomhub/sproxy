# 审查：v2 Web UI 全功能对抗审查（批次 1）

- **审查基线**：master `c2f99992a`
- **范围**：`web/static/v2/*`（22 个模块）+ 其调用的后端端点契约
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性
- **方法**：逐文件源码审查 + 端点契约核对（`docs/api.md`、`pkg/**` 实现）+ 聚焦测试 + e2e

## 结论

**总评**：有条件通过（发现 **2 个 P1、4 个 P2**，均已修复并有测试/变异证据）。

发现数：**P0 0 / P1 2 / P2 4 / P3 3**

## 发现清单

### [P1] 搜索结果行名已含目录前缀，前端再拼 subdir ⇒ 详情/预览/下载/删除/重命名全部 404

- **位置**：`files-view.js:97`、`files-actions.js:216/252/260/274/286`、`files-inspector.js:19`、`preview.js:47/147`
- **问题**：服务端 `GET /api/files/search` 返回的 `name` 是**租户根相对路径**（如 `v2demo/e2e.txt`），
  而 `joinSubdir(subdir, name)` 会拼成 `v2demo/v2demo/e2e.txt`。在子目录内搜索后，任何行内动作都
  指向不存在的路径：UI 表现为「加载失败/失败」，用户可能误以为文件损坏。
  （删除不会误伤其它文件——路径不存在 ⇒ 400/404，属可用性缺陷而非数据风险。）
- **验证**：`node --test web/static/v2/files.test.js` 新增 `resolveRel` 用例；
  e2e 在子目录内搜索 → 打开详情 → 断言路径为 `v2demo/e2e.txt` 且**不含** `v2demo/v2demo`；
  **变异命中**：把 `resolveRel` 改回 `joinSubdir` ⇒ e2e 在「搜索态详情路径拼接错误」处红，还原后绿。
- **修复**：新增 `v2files.resolveRel(subdir, name, isSearch)`，搜索态不再拼 subdir。

### [P1] 搜索态重命名会跨目录移动文件

- **位置**：`files-actions.js:272~281`
- **问题**：`to = joinSubdir(sub, newName)` 用当前 subdir，而搜索态源文件可能在更深目录
  ⇒ 重命名实际变成「跨目录移动」，与用户预期（同目录改名）不符。
- **修复**：目标目录从源路径派生（`from` 的 dirname），与源同目录改名。

### [P2] 「未完成上传」百分比恒为 0%

- **位置**：`upload-chunked.js:113`（`received: 0` 写死且不再更新）、`pending-upload.js:78`
- **修复**：新增 `v2pending.updateProgress(loaded, total)`；分块进度与断点续传初始进度都写回记录，
  刷新后提示显示真实「已接收 N%」。

### [P2] 审计导出扩展名与实际内容不一致

- **位置**：`audit-console.js`（`.ndjson`）、`index.html` 按钮文案
- **问题**：服务端 `/api/audit/export` 返回 **JSON 数组**（`sendJSONResponse`），前端命名 `.ndjson`
  ⇒ 下游按 NDJSON 逐行消费会失败。
- **修复**：文件名改 `.json`，按钮与 toast 文案同步为「导出 JSON」。

### [P2] 事项「打包已完成」在无任务组时直接失败

- **位置**：`console.js::archiveCloudIssue`
- **问题**：无 `group_id` 时提示「请到传输页打包」——但服务端**有**单任务归档端点
  `POST /api/cloud/tasks/{id}/archive`，能力白白缺失。
- **修复**：无组时回落单任务归档。

### [P2] 检查器 Meta 编辑框切换文件后残留上一文件内容

- **位置**：`files-inspector.js::open`
- **问题**：`state.loaded={}` 会重拉 meta，但编辑框只在为空时填充 ⇒ 打开文件 B 时仍是 A 的 extra，
  一键保存会把 A 的字段写进 B（数据污染风险）。
- **修复**：切换文件时清空编辑框，加载后覆盖填充。

### [P3] 页面卸载时轮询定时器未清理

- **位置**：`transfer.js`（自动刷新 5s、链式轮询 3s）
- **修复**：`beforeunload` 统一清理。

### [P3] 预览下载未 `revokeObjectURL`

- **位置**：`preview.js::download`
- **修复**：2s 后 revoke（与其他下载一致）。

### [P3] `prompt()`/`confirm()` 用于 Secret 与危险操作

- **位置**：`volumes-console.js`、`trash.js`、`mesh-console.js`
- **判断**：可用性可接受（原生阻塞对话框），但不满足一致的 a11y/样式；本批**不改**，
  避免与 i18n 词条重复施工。记录待改进。

## 通过项（已核验无问题）

| 面 | 结论 |
| --- | --- |
| 路径注入 | 用户输入经 `encodeURIComponent` 进 query；服务端 `ValidateFilePath` 兜底；`joinSubdir` 过滤 `..`/绝对路径 |
| XSS | 动态内容一律 `textContent`；3 处 `innerHTML` 均为静态模板（弹窗骨架、自动刷新 pill）+ 本地生成二维码 SVG |
| 权限 | 踢节点先探测 admin（`GET /api/credentials` admin-only）；审计/事项有服务端 403 兜底 |
| 破坏性操作 | 删除走软删入回收站；清空回收站/删卷/删 secret/踢节点均二次确认 |
| 轮询 | 自动刷新仅在当前分区可见时执行；链式轮询切走即停 |
| 凭证 | 仅写旧版同三键 `sessionStorage`，失败回滚；不落 localStorage |
| 资源上限 | 文本预览 512 KiB 截断；审计 500 条上限；任务列表 200 条 |
| 空态/错误态 | 每个分区都有空态与错误提示（服务端文案透传） |

## 验证方式

```
node --test web/static/v2/files.test.js            # 新增 resolveRel 用例
make web-test                                      # exit 0
go test ./internal/archcheck -run 'TestWebAssets|TestSleepRatchet|TestR55'
go build ./...
go test ./web/e2e -run TestV2Shell                 # 含「搜索态详情路径不得重复拼接」
golangci-lint run web/e2e/...                      # 0 issues
```

变异证据：`files-inspector.js` 的 `resolveRel` 改回 `joinSubdir` ⇒ e2e 红；还原后绿。

## 未核查面（明确声明）

- 后端端点自身正确性（本次只核前端调用契约；后端由各自包测试覆盖）；
- 浏览器兼容矩阵（仅 Chromium 实测；File System Access 自动续传仅 Chromium 可用）；
- 真实外部云盘编排（e2e 用 loopback 源替代 pikpak/百度）。

---

## 第二批（2026-11-25 续）：文件/目录全套操作 + 预览关闭交互

### 能力补齐

| 项 | 结论 |
| --- | --- |
| 目录重命名/移动 | **后端原不支持**（`RenameFile` 先要求 checksum，目录恒 400/校验失败）。新增 `renameDirInHome`：整棵子树搬迁 + meta 子树联动 + checksum 台账前缀改名 + 跨 Scope 配额转移；`RenameFile` 按 `Lstat` 自动分派，目录**不需要** checksum（TDD 4 例：子树/409/meta 子树/台账前缀） |
| 台账前缀改名 | 新增可选能力 `checksum.PrefixRenamer.RenamePrefixInVolume`（`ChecksumStore` + `StateBackedChecksumStore` 双实现 + 单测）——不实现的话目录改名后列表 checksum 全丢，后续删除/分享会恒失败 |
| 重命名即可移动 | 目标输入支持相对路径（`a/b/new`）⇒ 同时移动到该目录并自动建目录；纯逻辑 `v2ops.validateRelativePath`（拒绝 `..`/空段/保留字符）+ 单测 |
| 目录打包下载 | 目录行新增「打包下载」（`GET /api/archive-dir`），此前只有文件/多选打包 |
| 预览关闭 | 新增悬浮关闭按钮（视频控件/全屏遮挡时仍可关）、头部「下载」按钮、点击舞台空白关闭；云任务预览下载走 `kind=cloud_task` |

### 本轮修复的交互缺陷（用户反馈）

**视频预览只能 Esc 退出**：头部关闭按钮在视频元素/原生控件层之下，且没有其他关闭点。
修复：`.preview header { z-index: 3 }` + 独立 `.pv-float-close`（固定在右上、高 z-index）+ 舞台空白点击关闭 + 头部「关闭 ✕」文案明确快捷键。

### 验证

- `go test ./pkg/files`、`./pkg/checksum`、`./pkg/server`、`./internal/archcheck` 全绿；
- `make web-test` exit 0；`go test ./web/e2e -run TestV2Shell -count=2` 全绿（含目录重命名→内容随迁、
  悬浮关闭、舞台空白关闭、第三次打开用头部关闭）；
- `golangci-lint run pkg/files/... pkg/checksum/... web/e2e/...` 0 issues。

### 已知边界

- 目录重命名的**外部卷**（加密/网盘）仍走 `renameExternalVolume` 单文件语义 ⇒ 目录不支持（服务端接缝只有文件级 `ExternalRename`）；
- 目录重命名的配额转移在「目标 Scope 预留失败」时只记 Warn 不回滚（改名已成功，差分由对账收敛）——
  与 rmdir 的释放口径一致，但会在账本上留下待收敛偏差。
