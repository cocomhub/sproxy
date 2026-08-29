# Web 传输管理器（下载/上传管理模块）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法。
> 关联规格：`docs/superpowers/specs/2026-08-27-transfer-manager-design.md`；分支 feature/mesh-tunnel；**不用 worktree**，直接在当前分支开发。

**目标：** 把 Web 端的下载/上传统一成类网盘的「传输管理器」：下载有进度、刷新保留、恢复时按大小+时间校验自动续传免重选；按状态频道筛选（设计阶段即纳入）；已完成默认折叠可删记录/打开存储目录；云任务/云组迁移进传输页。

**架构：** 前端新增 `transfer-store`（localStorage 主列表 + IndexedDB 下载块缓存 + 上传文件句柄库）与 `download.js` 下载管线；`upload.js` 会话层改写为 TransferItem + 文件句柄；`app-render.js` 添加统一行渲染与 `filterTransferItems` 纯函数；index.html 增加顶部主 tab「文件/传输」并移除 cloud-modal；服务端新增 `GET /upload/sessions`。

**技术栈：** 原生 JS（UMD 兼容 Node 测试）、IndexedDB（promisify 封装）、File System Access API（选项）、Go 标准库。禁止引入第三方依赖。

---

## 文件结构与职责

- **创建 `pkg/server/chunked_upload_sessions_test.go`** — 会话列表端点测试（纯 stdlib）。
- **修改 `pkg/server/upload_store.go`** — 新增 `ListSessions()` 返回未完成会话快照。
- **创建 `web/static/transfer-store.js`** — 数据传输层：TransferItem localStorage 读写、IndexedDB 分块缓存（注入式 IDB backend 以便单测）、上传文件句柄库。UMD；顶部无 DOM 副作用。
- **创建 `web/static/download.js`** — 下载管线（新建/恢复/暂停/取消/完成合并），会话回调风格，纯计算与 DOM 分离。
- **修改 `web/static/app-render.js`** — 新增 `buildTransferRowHtml`/`buildTransferListHtml`/`filterTransferItems`（纯函数，导出）。
- **创建 `web/static/transfer-store.test.js`**、**`web/static/transfer-render.test.js`**、**`web/static/download.test.js`** — node:test。
- **修改 `web/static/index.html`** — 顶部主 tab 条、`#transfer-page`（频道条+URL 区+列表）、script 顺序加 transfer-store.js/download.js；移除 cloud-modal。
- **修改 `web/static/app.js`** — 主 tab 切换、传输页渲染、频道点击、云端逻辑迁入传输页（保留预览/链式/组流程）、移除 cloud-modal 事件、下载入口改传 `download.js`、`navigateDir + toast` 打开本地存储目录。
- **修改 `web/static/upload.js`** — 会话持久化改 transfer-store；文件句柄存取；续传状态机适配。
- **修改 `Makefile`** — `web-test` 加入新文件 `node --check` 与测试。
- **修改 `config_api.go`**（只读勘查，可能不需改）— `uploads_dir` 已存在于 `/api/config` 响应（`config_api.go:67`），前端 `sc.config.get()` 可拿到。

---

## 任务 1：服务端 `GET /upload/sessions`（列未完成上传会话）

**文件：**
- 修改 `pkg/server/upload_store.go`
- 修改 `pkg/server/chunked_upload.go`
- 修改 `pkg/server/handlers.go`（路由）
- 创建 `pkg/server/chunked_upload_sessions_test.go`

- [ ] **步骤 1：写失败的测试** `chunked_upload_sessions_test.go`：建 testUploadStore（复用 `chunked_upload_test.go:44-51` 模式），init 一个会话、传 1 块，断言 `GET /upload/sessions` 返回该会话（uuid/文件名/总量/已收块数/checksum）；complete 后返回空。用 `httptest` + 临时 uploadsDir。

- [ ] **步骤 2：跑测试确认失败** `go test -count=1 -run TestUploadSessions ./pkg/server/...` → 编译错误（无 handler）。

- [ ] **步骤 3：实现 `UploadStore.ListSessions()`** `upload_store.go`：加 `ListSessions() []ChunkedUploadSessionMeta`（不含 bitmap 但含 received_count、missing_count；从 `sessions` map 拷贝快照 + 计数），或返回浅拷贝列表。

- [ ] **步骤 4：实现 handler `uploadSessions`（chunked_upload.go）**：`GET /upload/sessions` → 遍历 `store.ListSessions()` 归一为 `{success:true, sessions:[{upload_id,filename,total_size,received_count,total_chunks,file_checksum,file_mod_time,status}]}`；异常返回 `{success:false,message}`。

- [ ] **步骤 5：注册路由（handlers.go `RegisterRoutes`）**：localMux（~143-146 附近）与 srvMux（~171-174 附近）各加 `mux.HandleFunc("GET /upload/sessions", auth.limit(h.uploadSessions))`（对齐 `GET /upload/status` 的注册方式）。

- [ ] **步骤 6：跑测试通过**，`golangci-lint run ./pkg/server/...` 0 issues。

- [ ] **步骤 7：Commit** `feat(server): GET /upload/sessions 列出未完成上传会话`。

---

## 任务 2：前端数据层 `web/static/transfer-store.js`

**文件：**
- 创建 `web/static/transfer-store.js`（UMD）
- 创建 `web/static/transfer-store.test.js`
- 修改 `web/static/index.html`（script 顺序加入，先于 download.js/upload.js；放在 cloudfilename.js 之后、upload.js 之前）
- 修改 `Makefile`（web-test 加 node --check + 测试行）

**约定（与 spec 一致）：** localStorage key `sproxy_transfer_items`；IndexedDB 库 `sproxy-dl-cache` / 仓库 `chunks` 主键 `[itemId, chunkIndex]`；库 `sproxy-up-dev` / 仓库 `fileHandles` 主键 `uploadId`。旧 `sproxy_upload_sessions` 不读、不迁移。

- [ ] **步骤 1：写失败的测试** `transfer-store.test.js`（注入 mock 实现）——测 `upsertItem`/`loadItems`/`removeItem`、`saveChunk`/`listChunkCount(itemId)`、`saveFileHandle/queryFileHandlePermission`。IndexedDB 提供者以参数注入 `createTransferStore({ls, idb})`；Node 测试用内存 fake。

- [ ] **步骤 2：跑测试确认失败** `node --test web/static/transfer-store.test.js` → Module not found。

- [ ] **步骤 3：实现 `transfer-store.js`**
  - UMD：浏览器挂 `root.transferStore`（`transferStore`），Node module.exports。
  - 纯函数：`normalizeItems`（JSON 容错返回 []）、`computeChunkIndex`、`chunkCountOf`。
  - 会话 API（localStorage 封装，容错 catch）：`loadItems()/saveItems(items)/upsertItem(item)/removeItem(id)`。
  - IDB 封装（promisify，统一 `_idbRequest`）：`openDB`、`saveChunk`、`listChunkCount`、`loadChunk`、`deleteChunkRange`、`saveFileHandle`/`getFileHandle`。
  - `createTransferStore({ls, idb})` 返回上述方法；浏览器默认 `ls=window.localStorage`、`idb=window.indexedDB`。

- [ ] **步骤 4：跑测试通过** + `node --check web/static/transfer-store.js`。

- [ ] **步骤 5：Commit** `feat(web): 传输数据层 transfer-store（TransferItem/IndexedDB 块缓存/文件句柄）`。

---

## 任务 3：app-render.js 传输渲染纯函数 + 按状态筛选

**文件：**
- 修改 `web/static/app-render.js`
- 创建 `web/static/transfer-render.test.js`
- 修改 `Makefile`

- [ ] **步骤 1：写失败的测试** `transfer-render.test.js`：
  - `filterTransferItems(items, channel)` 全频道（all/uploading/downloading/cloud_tasks/cloud_groups/completed）。
  - `buildTransferRowHtml(item)`：包含 `data-item-id`、进度文案、状态徽章（statusText）、操作按钮（暂停/恢复/取消/删除/打开存储目录）。

- [ ] **步骤 2：跑测试确认失败** `node --test web/static/transfer-render.test.js`。

- [ ] **步骤 3：实现 app-render.js 内的新纯函数**
  - `TRANSFER_CHANNELS` + 每频道筛选谓词。
  - `filterTransferItems(items, channel)`。
  - `buildTransferRowHtml(item)`：统一行（kind 图标 + filename + statusText + 进度条/百分比 + 操作按钮组）；上传含「已缓存 X/Y 块」；下载含「重新下载」；completed 折叠行由 `buildTransferListHtml` 处理。
  - `buildTransferListHtml(items, channel)`：过滤 + 已完成折叠（按 kind 分组 summary 行，点击展开 `group-detail-*`）+ 空列表文案「暂无传输记录」。

- [ ] **步骤 4：跑测试通过** + `make web-test` 全绿。

- [ ] **步骤 5：Commit** `feat(web): 传输渲染与按状态筛选纯函数（app-render）`。

---

## 任务 4：index.html 传输页结构与主 tab 导航

**文件：**
- 修改 `web/static/index.html`
- 修改 `web/static/app.js`（导航切换渲染入口）

- [ ] **步骤 1：实现 index.html**：在 `<h1>` 之后 toolbar 之上加主 tab 条 `<div id="main-tab-bar"><button id="main-tab-files" class="main-tab active">文件</button><button id="main-tab-transfer" class="main-tab">传输</button></div>`；把原 container 内容包一层 `#files-page`；新增 `#transfer-page`（频道条 + URL 输入区 + `#transfer-body`）。脚本顺序：transfer-store.js/download.js 加在 cloudfilename.js 后、upload.js 前。移除 `cloud-modal`（原 78-106 行整块）——URL 区迁到 transfer-page。

- [ ] **步骤 2：实现 app.js 导航**：`switchMainTab('files'|'transfer')`（隐藏/显示 pages + main-tab 激活样式）；`showTransferPage()`（进入时渲染列表 + 启动轮询）；`hideTransferPage()`（停止轮询）；频道点击委托到 `#transfer-channel-bar`；初始默认 files。

- [ ] **步骤 3：跑 `make web-test`** + 手工 Chrome 验证主 tab 切换。

- [ ] **步骤 4：Commit** `feat(web): 传输页导航骨架（文件/传输主 tab）`。

---

## 任务 5：云端逻辑迁入传输页、移除 cloud-modal

**文件：**
- 修改 `web/static/app.js`

- [ ] **步骤 1：`showCloudDownload`/`hideCloudDownload` 改传输页**：云下载按钮点击 → `switchMainTab('transfer')` + 频道切 `cloud_tasks`。移 `cloud-url` 输入区事件绑定（`bindCloudUrlRowEvents`）到 transfer-page；`startCloudPolling`/`stopCloudPolling` 改由传输 tab 激活/隐藏门控。

- [ ] **步骤 2：删除 cloud-modal 事件绑定**（~1473-1478 cloud-close/refresh/tasks-tab/groups-tab 等）。`showCloudDownload`/`hideCloudDownload`/`switchCloudTab` 改写为传输页频道切换适配。

- [ ] **步骤 3：`refreshCloudTasks`/`refreshCloudGroups` 渲染进传输页列表**：body 指向 `#transfer-body`（云任务/组转 TransferItem 或直接渲染表格；传输页统一渲染管）。

- [ ] **步骤 4：运行 `make web-test` + 浏览器验证**：下载、上传、组、任务列表都在传输页呈现；点「云端下载」切到传输 tab。

- [ ] **步骤 5：Commit** `refactor(web): 云任务/云组迁入传输页，移除 cloud-modal`。

---

## 任务 6：下载管理管线 `web/static/download.js`

**文件：**
- 创建 `web/static/download.js`（UMD）
- 创建 `web/static/download.test.js`
- 修改 `web/static/app.js`（downloadFile 入口改委派 + triggerDownload 复用到完成合并）
- 修改 `Makefile`

- [ ] **步骤 1：写失败的测试** `download.test.js`（mock transport coreRequest + mock IDB backend）：
  - `startDownloadItem` 创建 `kind:'download'` item 与 chunk 请求路径计算；
  - 完成路径全块合并 → Blob → 校验（header checksum 对比）→ 触发保存（onComplete 桩 + 缓存清理断言）；
  - 暂停中断在途（AbortController）；恢复只拉缺失块；
  - 恢复校验 stat HEAD X-File-MTime/Checksum 不匹配 → onMismatch。

- [ ] **步骤 2：跑测试确认失败**。

- [ ] **步骤 3：实现 `download.js`**：`createTransferStore` + `startDownload(filename, {size,mtime,checksum})` → 分块请求（并行 3、AbortController、每块写 IDB + upsertItem）→ `assembleAndSave`（读全块合并 → Blob → onComplete(blob,filename) 由 app.js 提供 triggerDownload+toast）→ 清块缓存 → completed。恢复 `resumeDownload(item)`：stat 取 size/mtime/checksum 比对 → 匹配只拉缺失块 / 不匹配 onMismatch（提示文件已变更，可选强制重新下载）。paused/failed 写回；取消清 IDB 块 + remove。

- [ ] **步骤 4：跑测试通过 + `make web-test` 全绿**。

- [ ] **步骤 5：Commit** `feat(web): 下载管理管线（分块缓存+恢复校验）`。

---

## 任务 7：上传会话层改写 `web/static/upload.js`

**文件：**
- 修改 `web/static/upload.js`
- 修改 `web/static/sclient/api/files.js`（如需要给 onSession 传递 fileHandle 不破坏签名，扩展 opts 内部即可）
- 修改 `Makefile`（upload.test.js 已有）

- [ ] **步骤 1：写失败的测试** `upload.test.js`（Node，mock transfer-store + files.upload 注入）：上传成功写 TransferItem、句柄缺失回落重选提示、恢复时校验 size/mtime 后只补缺失块。

- [ ] **步骤 2：跑步确认失败**。

- [ ] **步骤 3：实现 upload.js 改写**：
  - 替换 SESSIONS_KEY 体系：`loadSessions/saveSessions/saveUploadSession/removeUploadSession` → 委托 `transferStore.upsertItem/removeItem`（`kind:'upload'`），`resumedChunkCount` 改用 chunk bitmap；
  - `chunkedUpload` 的 `onSession` 沿用回调（files.js persist 钩子），data 存为 transferStore item；文件选择成功后当 FS API 可用且授予读取权限时把 `{fileHandle}` 存 IndexedDB `sproxy-up-dev`；
  - `checkResumableUploads` → 读取全部 upload 类 item → `/upload/status?upload_id=` 探测（命中 completed 删、missing>0 提示续传、其它删）；
  - `resumeUpload(uploadId)`：优先取 fileHandle → queryPermission('read') → getFile() → size 校验 → chunkedUpload 只补缺失块；句柄不可用 → 弹文件选择框（免重选仅 FS API 生效）。

- [ ] **步骤 4：跑测试 + `make web-test` 全绿 + 浏览器手动验证**（真实上传中断续传、句柄路径、回落路径）。

- [ ] **步骤 5：Commit** `feat(web): 上传会话层改 TransferItem+文件句柄`。

---

## 任务 8：服务端其余小缺口核查 + 全文验证

- [ ] **步骤 1：核查**：`/download`（download_handler.go:89 ServeContent）已支持 Range；`/download/chunk` 支持 offset 边界——无需新增。确认 localStorage 每域独立满足 spec。

- [ ] **步骤 2：全量验证**（提交前规约）：`golangci-lint run ./pkg/... ./cmd/... ./pkg/tunnel/xfer/ext/...`；`go test -count=1 ./pkg/server/...`；`make web-test`（含新增 3 个 node:test 全绿）；`make build` 通过。

- [ ] **步骤 3：E2E 新增场景（test/e2e_test.go）**：上传会话恢复（init 中断 → 断言 /upload/sessions 存在 → 恢复 → 完成 → 为空）、恢复校验 mtime 变更回退。无法自动的部分列入人工清单。

- [ ] **步骤 4：Commit** `test(e2e): 传输管理器会话恢复覆盖`。

---

## 人工验证清单（浏览器）

1. 真实上传大文件 → 中断 → 刷新 → 恢复（FS API 句柄路径与回落路径各一次）。
2. 真实下载大文件 → 中断 → 刷新 → 恢复 → 大小/mtime 匹配自动拉缺失块 → 完成合并保存。
3. 批量/目录打包下载纳入下载管理；云组下载归档纳入。
4. 频道筛选（全部/上传/下载/云任务/云组/已完成）切换无重拉。
5. 已完成默认折叠 + 展开 + 删除记录 + 打开存储目录（切回文件 tab + toast 服务端路径）。
6. console 无 error（上传、下载、列表、分享、版本弹窗、频道切换全覆盖）。
