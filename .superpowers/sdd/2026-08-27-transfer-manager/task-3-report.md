# Task 3 Report — app-render.js 传输渲染纯函数 + 按状态筛选

> 日期：2026-08-28 ｜ 分支：feature/mesh-tunnel ｜ 状态：DONE

## 提交

- hash：见 git log（本任务 commit，Conventional Commits）
- message：`feat(web): 传输渲染与按状态筛选纯函数（app-render）`
- 改动文件：3
  - `web/static/app-render.js`（修改，新增 transfer 渲染 + 筛选纯函数并导出）
  - `web/static/transfer-render.test.js`（新建，node:test，16 用例）
  - `Makefile`（web-test 并入 transfer-render.test.js）

未动其它 Web 文件：app.js / upload.js（upload.js 工作区已有修改属前任务遗留，本任务不触碰）。

## 核心实现（web/static/app-render.js）

统一渲染管线的三个导出 + 频道定义：

- `TRANSFER_CHANNELS`：6 个频道的 id/label 精确取值（全部/上传中/下载中/云任务/云组/已完成），顺序按 spec 不可打乱（供频道条渲染与切换高亮）。
- `_channelPredicates` 内部分发表 + `filterTransferItems(items, channel)`：
  - all → 全量；channel 为 null/undefined → 回落 all；
  - uploading → kind==='upload' 且 status ∈ {hashing, uploading, paused, failed, cancelled}（仅上传类，含失败/取消）；
  - downloading → kind==='download'（archive 同 download）且 status ∈ {downloading, paused, failed, cancelled}；
  - cloud_tasks / cloud_groups → 按 kind 全量透传（含 completed）；
  - completed → status==='completed' 全 kind 命中（upload/download/cloud_task/cloud_group 的 completed 都算）。
  - 未知频道 fail-closed 返回 []（防频道条外鉴权遗漏时静默渲染全部）。
- `buildTransferRowHtml(item)`：统一行（kind 图标 + filename + 状态徽章(statusText) + 进度条/百分比 + 操作按钮组），data-item-id 置于行根与每个按钮（全局可寻址）。已缓存块数由 `meta.chunksBitmap` 置位合计 →「已缓存 X/Y 块」；`buildProgressBar` 复用（进行中/计算态 total>0 才渲染）。按钮状态机：
  - hashing/uploading/downloading/pending → 暂停 + 取消；
  - paused → 恢复 + 取消；failed/cancelled → 恢复 + 删除记录（仅 upload/download 有恢复，云行终态仅删除）；
  - completed → upload「打开存储目录」/ download「重新下载」+ 删除记录。
- `buildTransferListHtml(items, channel)`：过滤 → 空列表「暂无传输记录」；已完成按 kind 分组折叠（`<details>`/`<summary>`，detail id `group-detail-*` 默认无 open 属性即折叠），运行项平铺前置、完成组后置；无完成项时整表平铺（避免空 details 占行）。

## TDD 证据

红：`node --test web/static/transfer-render.test.js` → 16 全 fail（TypeError: r.filterTransferItems is not a function / r.TRANSFER_CHANNELS undefined）——缺实现。

绿后 2 个 wrong-red 修正（行为决策，非实现缺陷）：
1. uploading/downloading 频道的 completed 项被筛选排除（spec-defined）→ `buildTransferListHtml` 在该频道下不产生完成折叠组；测试原断言折叠组存在，改为断言「不泄漏其它 kind」（实现不变，测试修正表达 spec）。
2. `filterTransferItems` 空字符串 `''` 从回落 all 改为 fail-closed []——已注释：空字符串是非法频道，缺省仅针对 null/undefined；UI 缺省以显式 'all' 进入（杜绝频道条空白时误渲染全部）。

## 验证命令 + 输出

```bash
node --test web/static/transfer-render.test.js
ℹ tests 16 / pass 16 / fail 0
make web-test     # 全绿：cloudfilename 6 + transfer-store 13 + transfer-render 16 + app-render 20 + upload 12 + sclient 45 = 112 tests 0 fail
```

## 自审

- 纯函数隔离：新函数零 DOM/零全局读，仅依赖文件内既有 `formatSize/escHtml/statusText/buildProgressBar`；无顶层副作用，Node require 安全。
- 未在 app.js/upload.js 重复定义同名函数（按 singular 源约定，调用方走 appRender.*）。
- 每种 HTML 属性值（id/filename）均经 escHtml 转义；文本经 escHtml 输出；测试覆盖含 `<` 的 XSS 样例。
- 语义精确对齐任务简报/规格（上传频道集合、downloading 含 archive、completed 全 kind、UI 标签逐字）。

## 疑虑

无未决。
