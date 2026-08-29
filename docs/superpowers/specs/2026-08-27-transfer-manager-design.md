# Web 传输管理器（下载/上传管理模块）设计

> 日期：2026-08-27 ｜ 分支：feature/mesh-tunnel

## 背景与问题

当前 Web 端文件下载是一次性、全内存、无进度、无历史、无断点（`app.js downloadFile`）：大文件占用大量内存、失败只能全部重来。上传虽有断点续传，但刷新后必须**手动重新选择文件路径**且会话状态依赖脆弱的猜测（存在 `localStorage['sproxy_upload_sessions']`、`/upload/status` 探测的缺失/完成/删除分支），多次"恢复"仍勉强可用而体验差。下载/上传/云任务/云组各自为政（下载无状态；上传进度条 + 云模态弹窗两个子 tab）割裂。

目标：设计类似网盘的**传输管理器**——点击下载后显示进度与状态，刷新后状态保留，点击恢复时先校验文件大小与时间匹配后自动续传、无需重新选择路径；统一管理上传/下载/组下载/云任务，**按状态筛选是界面核心交互的一部分（在设计阶段即纳入）**，已完成项保留记录默认折叠，可删除记录、打开存储文件夹。

## 总体架构

- **单一数据模型 `TransferItem`** 统一五类呈现：`upload` / `download` / `cloud_task` / `cloud_group` / `archive`（归档型 = `download` + `meta.archive`）。
- **单一任务列表** localStorage key `sproxy_transfer_items`（替旧 `sproxy_upload_sessions`，**无过渡期**，提交后直接使用新键并移除旧键的无效内容）。
- **下载数据** IndexedDB 库 `sproxy-dl-cache`（对象仓库 `chunks`，主键 `[itemId, chunkIndex]`，值 `{data:ArrayBuffer, size}`）。
- **上传文件句柄** IndexedDB 库 `sproxy-up-dev`（对象仓库 `fileHandles`，主键 `uploadId`，值 `{fileHandle}`）。
- **单一渲染管线**：全部归一为 `TransferItem[] → buildTransferRowHtml(item)`（app-render.js 纯函数），统一行组件：状态徽章 + 进度条 + 操作按钮组。

```
TransferItem (localStorage 主列表)
  ├─ upload      meta:{uploadId, fileChecksum, totalChunks, chunkSize, serverChunkSize, chunksBitmap}
  ├─ download    meta:{mtimeNano, checksum, chunksBitmap, archive?}
  ├─ cloud_task  meta:{raw 云任务对象快照}
  ├─ cloud_group meta:{raw 云组对象快照}
  └─ archive     归档型下载（服务端生成 tar.gz 后当普通大文件下载；无时间戳保真，走打包开关）
```

## 分节 1：UI 与导航（按状态筛选在设计阶段即纳入）

顶部两主 tab：**文件**（现有一切：列表/搜索/批量/监控/版本/分享/主题/目录/上传按钮）与**传输**（新增整页）。

传输页自上而下：

1. **状态筛选频道条**是传输页核心导航（非事后追加）。频道即状态分组，缺省「全部」：

```
[ 全部 | 上传中 | 下载中 | 云任务 | 云组 | 已完成 ]   ← 频道切换只重渲染，不重新拉取
```

频道定义（纯函数 `filterTransferItems(items, channel)`，app-render.js 单测覆盖全部频道）：
- `all`：全部 item（含已完成）
- `uploading`：kind==='upload' 且 status ∈ {hashing, uploading, paused, failed, cancelled}（进行中+失败/取消上传），或 `cloud_group`…（不含）——**仅上传**
- `downloading`：kind==='download'（含 archive）且 status ∈ {downloading, paused, failed, cancelled}
- `cloud_tasks`：kind==='cloud_task'
- `cloud_groups`：kind==='cloud_group'
- `completed`：status ∈ 完成族（upload/download `completed`；云任务/组 `completed`）

点击频道：重设激活样式 + `#transfer-body.innerHTML = renderTransferList(filterTransferItems(items, channel))`，数据源不变、不重新拉取。

2. URL 输入区（自 `cloud-modal` 迁入，保留预览/链式/提交/组流程的按钮与逻辑复用）。
3. 列表区 `#transfer-body`：按当前频道渲染，**已完成项默认折叠**（折叠行按 kind 分组），可点击展开；每行操作：删除记录、**打开本地存储目录**（upload 完成项：切回文件 tab 并 `navigateDir(item 所在子目录)` + `refreshList()`，同时 toast 服务端绝对路径 `{uploads_dir}/{dirname}`，`uploads_dir` 取自 `/api/config` 下发）、暂停/恢复/取消。（下载项无此操作。）

移除 `cloud-modal` 及其专属事件、`云端下载` 工具栏按钮并入传输 tab。

## 分节 2：上传管理（免重选路径，结合平台能力）

改写 `web/static/upload.js` 会话层（保留 `sclient/api/files.js` 分块内核）：

1. 新建上传 = 现有行为（hashing 占位 → init → chunk → complete），但会话写 `sproxy_transfer_items`（`kind:'upload'`）+ **File 句柄**（FS Access API）存 IndexedDB `sproxy-up-dev`（键=uploadId）→ 刷新后 `queryPermission('read')` 通过 → `getFile()` 直接读原文件续传，**无需重选路径**；句柄不可得（非 Chromium/拒绝/不可用）回落现有「选择文件续传」input，仍只补缺失块。
2. 状态机（对齐现有时序）：`hashing` 保留 Item + `/upload/status` 探测 completed→宣告完成并清理/否则保持等待续传；`uploading` 每块成功更新 chunksBitmap；`already_exists`=完成；服务端会话失联（404/500/finished 且 missing 空）→ 删除 Item。

## 分节 3：本地下载管理（进度 + 刷新保留 + 免重选路径）

新增下载管理，沿用上传会话回调风格：

1. 新建下载（点击文件行下载按钮 → 写 TransferItem `kind:'download'` + `sc.files.stat` 取 size/checksum/mtime）→ `/download/chunk?offset=&length=` 逐块（并行 3）写 IndexedDB 并更新 loaded/chunksBitmap。
2. 刷新恢复：读 IndexedDB 缓存块 →「已缓存 X/Y 块」→ 点「恢复」先 `sc.files.stat` HEAD 比对：**大小 + 修改时间匹配** → 只拉缺失块；异动 → 提示「文件已变更」，可选强制重新下载；mtime 不可得 → 回退大小+checksum。
3. 完成：全块 merge → Blob → `triggerDownload` 保存 → 清该 item 的 IndexedDB 块 → completed。
4. 操作：暂停（中断在途请求）/恢复/取消（清两库）/删除记录/打开存储文件夹。
5. **时间戳保真（可选开关）**：下载区提供「保留原始时间（打包 tar.gz）」——勾选后单文件下载改走 `/api/archive` 打包保 mtime；未勾保持现有单文件下载。默认不勾。

## 分节 4：批量/组下载与云任务/云组落位

- 批量/目录打包下载 → `kind:'download'`、`meta.archive=true`、文件名 `.tar.gz`，走同一管线。
- 云组下载到本机归档复用现有链式流程，下载归档纳入 Item。
- 云任务/云组渲染进「云任务/云组」频道，沿用现有 3s 轮询与 `sc.cloud` 领域 API 渲染为统一行或复用 `buildCloudTaskTableHtml`/`buildCloudGroupTableHtml`。
- 移除 `cloud-modal` 与其专属事件；`switchCloudTab` 逻辑归入频道筛选复用。

## 分节 5：状态持久化明细

| 存储 | 主键 | 记录 |
|---|---|---|
| `sproxy-dl-cache`（库）`chunks` | `[itemId, chunkIndex]` | `{data:ArrayBuffer, size}` |
| `sproxy-up-dev`（库）`fileHandles` | `uploadId` | `{fileHandle}` |
| `sproxy_transfer_items`（localStorage） | itemId | TransferItem JSON |

**无过渡期**：直接使用 `sproxy_transfer_items`；旧 `sproxy_upload_sessions` 不读、不迁移，其无效残留由 `removeUploadSession` 语义覆盖（上传完成/失败即清除）。上传/下载每块成功原子 upsertItem（可合并 IndexedDB 事务）；传输页可见 1s 轻刷 + 云任务/组 3s 轮询仅传输 tab 激活时运行；已完成按 kind 折叠，可删除记录、打开存储文件夹。

## 分节 6：服务端 API 缺口与测试计划

- 缺口一：`GET /upload/sessions` 列出未完成上传会话（server 已有 `GetSessionByFilename`/`GetOrCreateSession` 内部对照，新增统一列出）。实现于 `pkg/server/chunked_upload.go` / `upload_store.go`，路由挂 srvMux 认证面 + localMux 隧道面。
- 缺口二：无（`/download/chunk` 已支持 offset/length；恢复校验走 HEAD stat）。
- 测试（纯 stdlib / 127.0.0.1 回环）：Go `pkg/server/chunked_upload_sessions_test.go`；JS `web-test` 新增 `transfer-store.test.js`（IndexedDB 封装纯函数）、`transfer-render.test.js`（行组件 + 按状态筛选纯函数）；E2E `test/e2e_test.go` 场景（刷新/恢复/已变更回退/块合并）；人工验证清单（真实上传中断续传、下载中断恢复、组/批量下载纳入、频道筛选、存储文件夹打开）。

## 状态枚举（跨 kind 统一）

`hashing` `uploading` `downloading` `paused` `completed` `failed` `cancelled` `pending` + 云任务/组原 status 透传。
筛选纯函数 `filterTransferItems(items, channel)` 由频道条驱动（属于分节 1 界面设计的一部分，单测覆盖全部频道）。
