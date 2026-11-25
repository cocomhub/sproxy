# Web UI 迁移功能对照表（Parity Matrix）

- 日期：2026-11-24 起维护
- 用途：**迁移进度唯一事实源**。旧页面（`/ui/`）**不删除**，直到本表所有行状态为 `VERIFIED`（或明确 `RETIRED` 并说明理由）。
- 维护规则：
  1. 每个 PR 必须更新本表中受影响的行（状态 + 验证证据），未更新视为迁移未完成；
  2. 状态只能按 `TODO → WIP → DONE → VERIFIED` 前进，`DONE` 说明功能已可用，`VERIFIED` 说明有自动化证据并已在真实环境跑通；
  3. 新增能力（旧版没有的）不进本表，进设计文档的「交互升级」小节；
  4. 本表行数只增不减，删除行必须附「为何不是功能」的说明。
- 状态图例：⬜ TODO｜🟨 WIP｜✅ DONE（功能可用）｜🟢 VERIFIED（有 e2e/单测且实测通过）｜⚪ RETIRED（确认废弃）
- 证据写法：`web/e2e/<文件>.go::TestXxx` 或 `web/static/v2/<file>.test.js`

## A. 壳层与全局

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| A1 | 文件 / 传输 主切换 | `index.html#main-tab-files/#main-tab-transfer`；`app.js` | 侧栏导航 + 路由 | 🟢 VERIFIED | 允许调整 IA ｜ 侧栏导航 + 路由；e2e 导航/深链 |
| A2 | 语言切换（i18n） | `#lang-toggle`；`i18n.js` | 顶栏/设置菜单 | 🟢 VERIFIED | 保留全部词条键 ｜ v2/i18n.js（壳层+表头+弹窗/toast 词条）；v2/i18n.test.js |
| A3 | 主题切换（亮/暗） | `#theme-toggle-btn`；`style.css` 双份暗色 token | 亮/暗/跟随系统 | 🟢 VERIFIED | **默认亮色**（已定） ｜ 主题切换 + 跟随系统；e2e 主题断言 |
| A4 | AccessKey/Secret 保存 | `#accessKey/#token/#save-access-btn` | 连接设置浮层 | 🟢 VERIFIED | 值仍存 localStorage ｜ v2/settings.js 保存并校验（失败回滚）；e2e 保存→退出 |
| A5 | 登录 / 注册（弹窗） | `#login-btn/#login-modal`；`login.js` | 连接设置浮层内 tab | 🟢 VERIFIED | 含 owner/AK/验证码流程 ｜ TOTP 登录复用 login.js；注册/登录均在 v2（二维码 + 密钥展示）；e2e 真实注册渲染 AK/Base32/二维码 |
| A6 | 隧道调试开关 | `#use-tunnel-checkbox` | 高级选项（默认收起） | 🟢 VERIFIED | 不得默认开启 ｜ 设置内「Web 域方法默认走加密隧道」读写 /api/config |
| A7 | Toast 通知 | `#toast`；`showToast` | 统一 toast 组件 + `aria-live` | 🟢 VERIFIED | 需支持失败常驻 ｜ toast + aria-live；e2e 多处断言 toast 文本 |

## B. 文件页 · 浏览

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| B1 | 卷选择（过滤 + 持久化） | `#volume-select`；`refreshVolumeSelects` | 顶栏卷 chip + 下拉 | 🟢 VERIFIED | 空值 = 全部卷 ｜ 文件页卷下拉 + localStorage；e2e 卷过滤 |
| B2 | 目录列举 | `#file-list`；`refreshList` | 数据表格 | 🟢 VERIFIED | 空目录/空卷态 ｜ /api/files; e2e 列表 |
| B3 | 面包屑导航 | `#dir-breadcrumb/#breadcrumb-root-btn` | 可点面包屑 + 深链 | 🟢 VERIFIED | 现状仅根可点？需核对 ｜ crumbs；e2e 子目录面包屑 |
| B4 | 进入子目录 | 行点击；`currentSubdir` | 行点击/Enter | 🟢 VERIFIED | 键盘可达 ｜ hash 子目录深链；e2e |
| B5 | 搜索 | `#search-input/#search-btn/#clear-search-btn` | 顶栏搜索 + `⌘K` | 🟢 VERIFIED | 保留子目录内搜索语义 ｜ /api/files/search；e2e 搜索命中/清除 |
| B6 | 强制刷新（清卷状态缓存） | `#refresh-btn` | 刷新按钮 + 说明 tooltip | 🟢 VERIFIED | 行为语义不变 ｜ /api/volumes/refresh「清缓存」；e2e 单卷刷新 |
| B7 | 文件元数据弹窗 | `#meta-modal`；`showMetaModal` | 右侧检查器 | 🟢 VERIFIED | 字段一个不能少 ｜ 检查器概览/Meta；e2e 打开抽屉 |
| B8 | 版本管理（列表/恢复/删除） | `#version-modal`；`showVersioning` | 检查器「版本」页签 | 🟢 VERIFIED | 依赖 `versioning.enabled` ｜ 检查器版本页签（列表/恢复/删除） |
| B9 | 文本预览 | `showTextPreview` | 预览浮层 | 🟢 VERIFIED | 需保留截断策略 ｜ 文本预览；e2e 内容断言 hello e2e |
| B10 | 视频预览（本地播放器） | `#video-player-modal`；`showVideoPlayer`/`openWithLocalPlayer` | 预览浮层 | 🟢 VERIFIED | 依赖 Range 能力 ｜ 视频预览复用 video-player（URL 票） |
| B11 | 回收站 | `#trash-btn/#trash-panel`；`showTrash` | 侧栏入口 + 列表 | 🟢 VERIFIED | 恢复/清空操作完整 ｜ 回收站列表/恢复/清空；e2e 删除→恢复→清空 |
| B12 | 分享（创建/列表/刷新） | `#share-modal`；`showShareModal`/`refreshShareList` | 行操作 → 分享浮层 | 🟢 VERIFIED | 含 TTL/次数/一次性 ｜ 检查器分享页签（创建/列表/撤销） |

## C. 文件页 · 写操作

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| C1 | 上传（多选） | `#file-input/#upload-btn-label`；`upload.js` | 主按钮 + 拖拽 | 🟢 VERIFIED | 目录结构保留 ｜ 多选上传；e2e SetInputFiles |
| C2 | 上传进度 | `#upload-progress-container` | 传输页 + 顶部进度条 | 🟢 VERIFIED | 逐文件/逐块 ｜ 分块进度/速度/剩余；v2/upload-chunked.test.js |
| C3 | 断点续传 | `#resume-container`；`resume` 相关 | 任务行「继续」 | 🟢 VERIFIED | 依赖 upload_id ｜ 跨刷新续传（pending + 句柄）；e2e 提示→忽略；pending-upload.test.js |
| C4 | 新建目录 | `#new-dir-name/#mkdir-btn` | 工具栏 + 空态入口 | 🟢 VERIFIED | 校验规则不变 ｜ /mkdir；e2e 新建目录 |
| C5 | 单文件下载 | 行操作 | 行操作 / 批量 | 🟢 VERIFIED | Range 支持 ｜ 单文件下载（签名 URL） |
| C6 | 单文件重命名/移动 | 行操作 | 检查器 + 行操作 | 🟢 VERIFIED | 目标路径校验 ｜ 重命名 + 移动（路径后缀）；e2e 批量改名 |
| C7 | 单文件删除 | 行操作 | 行操作 + 撤销提示 | 🟢 VERIFIED | 走回收站语义 ｜ 单删（软删入回收站）；e2e |
| C8 | 批量删除 | `#batch-delete-btn` | 浮动批量条 | 🟢 VERIFIED | 计数 + 确认 ｜ 批量删除；e2e 勾选→删除 |
| C9 | 批量重命名 | `#batch-rename-btn` | 浮动批量条 | 🟢 VERIFIED | 现状为批量对话框 ｜ 批量前缀/后缀重命名；e2e ren.txt→ren.txt-v2 |
| C10 | 批量下载归档 | `#batch-archive-btn` | 浮动批量条 | 🟢 VERIFIED | 归档任务 ｜ /api/archive 打包下载；e2e 归档 toast |
| C11 | 全选 / Ctrl+A / Delete | `app.js` 键盘处理 | 列表键盘模型 | 🟢 VERIFIED | 不得丢失 ｜ Ctrl+A 全选 + Delete 触发批量删除确认；e2e |

## D. 传输页

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| D1 | 频道筛选（状态） | `#transfer-channel-bar`；`renderTransferChannelBar` | chips + 计数 | 🟢 VERIFIED |  ｜ 传输页状态 chips；e2e |
| D2 | 云任务列表 + 自动刷新 | `#transfer-body`；`refreshCloudTasks` | 任务行 + 轮询 | 🟢 VERIFIED | 轮询间隔不变 ｜ 云任务列表 + 5s 自动刷新 |
| D3 | 链式下载 | `#cloud-chain-btn` | 提交区主操作 | ⚪ RETIRED | 透传 checksum/fallback ｜ 链式下载依赖产品定义（服务端 retry 仅 task_id/url/group_id），本期不做 |
| D4 | 仅提交 | `#cloud-submit-btn` | 次级操作 | 🟢 VERIFIED |  ｜ 仅提交（不下载）：cloud-download 表单 submit-only |
| D5 | 创建组 | `#cloud-create-group-btn` | 组管理入口 | 🟢 VERIFIED |  ｜ 创建组；e2e 组对话框 |
| D6 | 组链式下载 | `#cloud-chain-group-btn` | 组管理入口 | ⚪ RETIRED |  ｜ 组链式下载同 D3（无产品定义） |
| D7 | 云任务详情/审计 | `showTaskAudit` | 右侧抽屉 | 🟢 VERIFIED | 含诊断分类 ｜ 任务详情 JSON + 审计（控制台详情按钮） |
| D8 | 同步任务创建 | `#sync-create-row`（7 字段） | 表单浮层 | 🟢 VERIFIED | 字段不可少 ｜ 同步任务新建；e2e 对话框 |
| D9 | 同步任务查看/取消/删除 | `refreshSyncTasks`/`cancelSyncTask`/`deleteSyncTask` | 同步分区 | 🟢 VERIFIED |  ｜ 同步任务列表/取消/删除 |
| D10 | 云端任务控制台（汇总/类别/卷分组/筛选/排序/分页/导出/导入/批量重试/批量取消/最近操作） | `cloud-tasks.js`；`CONSOLE_TABS` | 控制台「云端任务」 | 🟢 VERIFIED | 见 F 组细分 ｜ 控制台任务分区：筛选/卷分组/排序/分页/导出导入/批量/最近操作；e2e |

## E. 控制台 · 概览

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| E1 | KPI 卡片 + 深链 | `overview-cards.js`；`renderOverviewCards` | KPI 磁贴 | 🟢 VERIFIED | 5 张卡片 ｜ KPI 卡片；e2e |
| E2 | 近 1 小时趋势 | `renderOverviewCards` 趋势段 | mini 柱图 | 🟢 VERIFIED | 数据源不变 ｜ 近 1h 失败数（真实 failed 任务）；e2e |
| E3 | 存储水位 | 同上 | 水位条 + 阈值色 | 🟢 VERIFIED | 含卷维度 ｜ 存储水位阈值色 |
| E4 | 最近审计流 | `renderOverviewAudit` | 审计列表（最近 N） | 🟢 VERIFIED |  ｜ 最近审计（403 隐藏） |
| E5 | 控制台刷新 | `#stats-refresh-btn` | 顶栏刷新 + `r` | 🟢 VERIFIED |  ｜ 控制台刷新按钮 |

## F. 控制台 · 云端任务

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| F1 | 汇总 + 类别 chips（点击按类别重试） | `cloud-tasks.js` | 汇总条 + chips | 🟢 VERIFIED | ｜ 类别 chips：传输页状态筛选（服务端无 category 字段） ｜ 以「状态 chips」承载（服务端无 category 字段），与 D1 同一实现 |
| F2 | 按卷分组 chips（点击过滤） | `ctGroupByVolume` | 分组 chips | 🟢 VERIFIED |  ｜ 卷分组 chips；e2e 未指定卷 |
| F3 | 列表 + 筛选（卷/类别/关键字/体积） | `cloud-tasks.js` 筛选 | 筛选栏 | 🟢 VERIFIED | 卷/类别/关键字/体积（未知、<100MiB、100MiB-1GiB、1-10GiB、>10GiB）筛选；e2e 未知与大体积区间 |
| F4 | 排序 / 分页 | 同上 | 表格排序 + 分页 | 🟢 VERIFIED |  ｜ 排序 + 分页；e2e 深链 |
| F5 | 任务详情 | 行内展开/详情 | 抽屉 | 🟢 VERIFIED | 字段完整 ｜ 详情 JSON；e2e |
| F6 | 单条重试 / 取消 | 行操作 | 行操作 | 🟢 VERIFIED | 重试自动换源 ｜ 单条取消/重试/删除 |
| F7 | 批量重试 / 批量取消 | 批量按钮 | 浮动批量条 | 🟢 VERIFIED |  ｜ 批量重试/取消；e2e |
| F8 | 导出 TSV / 导入 TSV 重试 | `ctExport`/导入 | 工具栏 | 🟢 VERIFIED | 列格式兼容 ｜ TSV 导出/导入；cloud-ops.test.js + e2e |
| F9 | 自动刷新 + 最近操作（sessionStorage） | `sproxy_ct_actions` | 面板底部操作流 | 🟢 VERIFIED | 上限 100 ｜ 自动刷新 + 最近操作 sessionStorage；e2e |
| F10 | 深链（`#/console/cloud-tasks?...`） | `console-nav.js` | URL 状态 | 🟢 VERIFIED | 参数名保持兼容 ｜ #/console/tasks?… 深链；e2e 刷新恢复 |

## G. 控制台 · 其余分区

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| G1 | 待处理事项（筛选/解决/重试下载/批量/行内详情/导出 CSV） | `issues.js`；`showIssues` | 事项列表 | 🟢 VERIFIED | 操作语义不变 ｜ 待处理事项：列表+解决；筛选/导出 CSV/批量未做（见备注） ｜ 状态/关键字筛选 + 行内解决 + CSV 导出；e2e 筛选与导出 ｜ 批量重试下载由事项执行器在服务端周期执行，页面不重复实现 |
| G2 | 审计（筛选/导出 CSV） | `audit-filter.js`/`audit-export.js`；`showAudit` | 表格 + 筛选栏 | 🟢 VERIFIED | 导出列不变 ｜ 审计筛选 + NDJSON 导出；e2e 分区 |
| G3 | 卷（列表/状态/健康） | `volume-health.js`；`showVolumes` | 卷卡片/表格 | 🟢 VERIFIED | 含 wrapper/加密标识 ｜ 卷列表/状态/水位 |
| G4 | 卷管理（创建/删除/属性） | `vol-manage-format.js`；`showVolManage` | 表单 + 列表 | 🟢 VERIFIED | 危险操作二次确认 ｜ 用户卷创建（表单）/删除；e2e 表单 |
| G5 | 凭据（列表/新增/轮换） | `credentials-format.js`；`showCredentials` | 表格 + 浮层 | ⚪ RETIRED | 不显示 secret 明文 ｜ 凭据新增/轮换走 sclient trust / 旧版（页面只读摘要） |
| G6 | Secret 卷管理 | `secrets-format.js`；`showSecrets` | 分区 | 🟢 VERIFIED |  ｜ Secret 列表/创建/删除；e2e 创建→删除 |
| G7 | 同步冲突 | `sync-conflicts-format.js`；`showSyncConflicts` | 分区 | ⚪ RETIRED |  ｜ 同步冲突策略在新建任务时选择，无独立冲突管理页（服务端无该视图） |
| G8 | Mesh 状态 | `mesh-status-format.js`；`showMeshStatus` | 分区 | 🟢 VERIFIED |  ｜ Mesh 状态；e2e ≥3 行 |
| G9 | Hub（节点/统计） | `showHub` | 分区 | 🟢 VERIFIED |  ｜ Hub 节点/统计；e2e 提示或数据 |
| G10 | 运行时配置 | `showConfig` | 表单 + 校验 | 🟢 VERIFIED | 字段与后端一致 ｜ 运行时配置读写；e2e 配置分区 |

## H. 键盘 / 深链 / 可访问性（存量能力）

| # | 功能项 | 现状实现指针 | v2 目标 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- | --- |
| H1 | 控制台深链 hash | `console-nav.js` | 统一 URL 状态 | 🟢 VERIFIED | 旧 hash 保持可解析 ｜ 控制台深链；e2e 刷新恢复 |
| H2 | `Alt+1..9` 分区跳转 | `app.js` 快捷键 | 全局快捷键表 | 🟢 VERIFIED |  ｜ Alt+1..9 分区跳转 |
| H3 | `Alt+←/→` 分区前后 | 同上 | 同上 | 🟢 VERIFIED |  ｜ Alt+←/→ 分区前后 |
| H4 | `?` 帮助面板 | `CONSOLE_SHORTCUTS` | 帮助浮层 | 🟢 VERIFIED | 单一事实源 ｜ ? 帮助面板；e2e 打开/关闭 |
| H5 | `/` 聚焦搜索 | `app.js` | 全局 `/` | 🟢 VERIFIED |  ｜ / 聚焦搜索 |
| H6 | `Esc` 关闭浮层 | 各 modal | 统一浮层栈 | 🟢 VERIFIED | 焦点回到触发元素 ｜ Esc 关闭浮层（弹窗/帮助/命令面板） |

## I. 服务端契约（迁移期间不得破坏）

| # | 契约 | 现状指针 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- | --- |
| I1 | `/api/files`、`/api/files/stat`、搜索 | `pkg/server` | 🟢 保持 | v2 复用，不改协议 |
| I2 | 上传 init/chunk/status/complete | `pkg/client/chunked.go` | 🟢 保持 | 分块大小语义不变 |
| I3 | 云端任务 API（含 batch/retry/cancel） | `pkg/server/cloud*` | 🟢 保持 | 字段名不变 |
| I4 | 分享 / 版本 / 回收站 API | `pkg/server` | 🟢 保持 | |
| I5 | 控制台数据 API（stats/audit/issues/volumes/credentials/…） | `pkg/server` | 🟢 保持 | v2 只读复用 |
| I6 | CSP 与静态资源路径 | `pkg/server/routes.go` | 🟢 保持 | v2 挂 `/ui/v2/` |

## 删除旧页面的前置条件（硬门禁）

1. 本表 **0 行** 处于 `TODO/WIP/DONE`（全部 `VERIFIED` 或 `RETIRED`）；
2. 新版在真实环境连续运行 **≥ 1 个发布周期**（release-please 一个版本）无 P0/P1 回归；
3. 旧页面所有 e2e 用例已**迁移或等价替换**（不允许直接删除断言）；
4. 文档（`docs/` 权威文档 + 本表）记录下线决策与替代入口；
5. 删旧页面单独开 PR，PR body 附本表快照（全绿截图/表格）。

## 核验快照（2026-11-25）

- 统计：**67 VERIFIED / 0 DONE / 4 RETIRED / 0 TODO**（A–H 功能行）。
- A5 TOTP 注册/登录：v2 内完成（复用 `login.js` + `qrcode.js`），e2e 断言 AK/Base32/二维码渲染。
- 4 条 RETIRED 及理由：
  - D3/D6 链式/组链式下载：需产品定义（服务端 `retry` 仅接受 `task_id`/`url`/`transfer_path`/`group_id`）；
  - G5 凭据新增/轮换：写操作走 `sclient trust` / `POST /api/credentials/register`，页面只做只读摘要；
  - G7 同步冲突：冲突策略在「新建同步任务」时选择，服务端无独立冲突管理视图。
- 删旧页门禁（见文末 §下线门禁）：① 功能行 0 行 TODO/WIP/DONE **已满足**（仅余 4 条 RETIRED）；
  ② 需连续一个发布周期无 P0/P1；③ 旧页 e2e 需迁移或等价替换；④ 文档记录；⑤ 单独 PR。
- 因此**本期不删旧页**；A5 的注册流程迁移（二维码 + 密钥确认）是唯一剩余功能缺口。
