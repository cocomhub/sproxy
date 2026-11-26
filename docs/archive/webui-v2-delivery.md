# Web UI v2 交付归档（2026-11-24 ~ 2026-11-25）

> 结论性知识，供后续维护者复用。**不含**操作步骤/PR 流水账（那些在 git 历史里）。

## 一、为什么做双轨而不是改造旧页

旧页 118 个 e2e id 与大量内联样式（700+ 处）耦合严重，直接改造会同时破坏 e2e 与视觉。
选择「旧页零改动 + 新页独立目录」后：灰度靠路由（`/ui/v2/`）、回退靠链接（顶栏「旧版」），
后端零开关、零协议变更。

## 二、关键设计判断（仍然有效）

1. **不引入前端框架与构建步骤**：CSP `default-src 'self'`，全部原生 ESM/UMD 脚本；代价是手写渲染，
   收益是无供应链与构建链风险、静态文件可直接审计。
2. **JS 不复制，改分层**：`shared/`（纯逻辑）+ `sclient/`（协议原语）+ `v2/`（视图）。旧页里可复用的
   纯函数模块（`cloud-tasks.js`、`overview-cards.js`、`video-player.js`）**直接引用**，不打包不复制。
   反例教训：早期 v2 曾复制 `formatSize`/`creds`，后统一收编进 `shared/`。
3. **认证复用旧版 SproxySig**：同源读 `sessionStorage` 三键，v2 不新增会话/不新增协议；
   TOTP 注册/登录复用旧版 `login.js` 的 nonce→login→解密流程，v2 只提供 `coreRequest` 适配器
   （否则旧模块依赖旧页 `app.js` 注入会失败）。
4. **删除语义统一为软删**：v2 单删/批量删都带 `?soft=true`，回收站才可用；硬删只在显式场景。
   踩过的坑：软删会释放 checksum 台账，而恢复早期只搬文件与 meta ⇒ 恢复出的文件**再次删除恒 400**；
   修复是恢复时用 sidecar 的 `TotalSHA256` 重建卷维度台账键。
5. **断点续传的现实边界**：浏览器不能持久化 `File`。方案 = 服务端确定性 `upload_id`
   （`filename|size|mtime|checksum`）+ `/upload/status.missing_chunks` 只补缺口；页面持久化 pending
   记录并提示「重选同一文件」；Chromium 用 File System Access 句柄（IndexedDB）自动续传。
6. **控制台页面化**：11 个分区 + hash 深链（`#/console/<section>?q=&status=&vol=&size=&sort=&order=&page=`），
   筛选/排序/分页即时写回，刷新恢复。**不要**再回到「一个弹窗塞所有 tab」。
7. **i18n 分层**：壳层/表头/弹窗/toast 走词条表（`data-i18n` + `ZH2EN` 静态表，原文存 `dataset.zh`
   保证可逆）；**动态数据保持服务端原文**，不伪造翻译。

## 三、踩坑记录（会再犯的）

| 坑 | 表现 | 正确做法 |
| --- | --- | --- |
| `maps.Copy` 复制 `map[string]*T` | `-race` 报 Add 写 vs json marshal 读（dedup 台账） | 锁内**深拷贝**（含切片副本）后再序列化 |
| 读-判断-插入型渲染无锁 | 概览卡片并发插入两份（e2e 计数 10≠5） | 入口防重入 + 抽 `...Once`，finally 复位 |
| 移动 DOM 后仍断言旧位置 | e2e 等待 30s 超时（元素已不在该分区） | 断言前显式 `location.hash` 回目标分区并等待容器可见 |
| `page.Locator().SelectOption` 返回双值 | Go 编译错（`1 variable but returns 2`） | `if _, err :=` |
| 测试轮询用 `time.Sleep` | 触发 R14 睡眠棘轮（archcheck 红） | `<-time.After(...)` |
| PR 与 base 冲突 | `gh pr checks` 恒 `pass=1 pending=0`，CI 不触发 | 先查 `mergeStateStatus=DIRTY` → rebase 再 push |
| stacked PR 基线被 `--delete-branch` 删 | GitHub 自动关掉依赖 PR | 先 retarget 再删，或合并后 rebase 重开 |

## 四、门禁与证据

- 功能对照：`docs/designs/webui-parity-matrix.md`（67 VERIFIED / 0 DONE / 4 RETIRED / 0 TODO）。
- e2e 等价映射：`docs/designs/webui-e2e-migration-map.md`（旧页 88 个 UI 用例逐条映射）。
- 旧页删除门禁：parity 全绿 + 一个发布周期无 P0/P1 + e2e 已迁移 + 文档记录 + 单独 PR。
- 自动化：`make web-test`（node --test，含 v2 纯函数）、`go test ./web/e2e -run TestV2Shell`、
  `internal/archcheck`（R10 资产登记 / R14 睡眠棘轮）。

## 五、仍开放

链式下载（需产品定义）、Hub 踢节点（管理写操作）、旧页下线（等发布周期）、
建卷类型探测（后端无「已注册类型」端点，下拉为静态列表）。

## 2026-11-26 UI 体验修复（PR 见 git 历史）

真机三视口（1440/768/390）+ DOM 探针实测发现并修复 5 类问题：

| # | 问题 | 根因 | 修复 |
| --- | --- | --- | --- |
| 1 | 「同步任务」卡片在**回收站/控制台**页面上也显示 | 该 `<div class="card">` 被写成 `</section>` 之外的**游离节点**（早期 PR 结构笔误），`data-panel` 切换只隐藏 panel 本身 | 挪回 transfer 面板内；新增 e2e 断言 `.content > .card` 必须为空 |
| 2 | ≤900px **完全无法切页** | `.rail{display:none}` 后没有替代导航 | 新增 `.mobile-nav`（横向 tab + 同一 `[data-route]`/`[data-badge]` 契约） |
| 3 | 窄屏横向撑破（contentScrollW 1036 > 390） | `.card-h` 无 `flex-wrap`；表格 551px 直接撑宽文档 | `.card-h` 加 `flex-wrap`；文件页工具栏独立成 `.card-tools` 行；≤900px `.table{display:block;overflow-x:auto}` |
| 4 | 侧栏徽标 `128 / 3 / 4` 是**假数据** | HTML 里硬编码，永不更新 | 新增 `web/static/v2/nav-badges.js`（纯函数 `badgeText`/`accountModel` + `v2:badge` 事件）；files/transfer/trash 视图广播真实计数；`>99` 折叠为 `99+`，未知隐藏、`0` 显示 |
| 5 | 过期文案与占位身份 | `v2 · P0`、`v2 预览` pill、「仍走旧版」banner（内容早已不成立）、账户卡写死 `demo · 示例凭据` | 删除 P0/预览 banner；账户卡由 `shared/creds.js` 读 sessionStorage 渲染（AK 只显示前 6 位 + `…`，匿名显示「匿名」） |

补充：
- 副标题 `TITLES` 去数字化（曾写死「/ 3 个进行中」「/ 保留 30 天」），改由视图经 `v2:subtitle` 实时写入；
  `node --test` 增加「副标题不得含数字」回归守卫。
- 命令面板 `goRoute` 改为只点**可见**的 `[data-route]`（侧栏+窄屏导航同名，`click` 到隐藏节点无意义）。
- 设置面板改为 `max-height:80vh; overflow:auto`——内容变长后此前**下方按钮点不到**（e2e 超时暴露）。

验证：`make web-test`、`go test ./web/e2e -run TestV2Shell -count=2`、`go test ./internal/archcheck`；
5 处修复各配**变异验证**（挪回游离卡片 / 删除窄屏导航 / 徽标写死 128 / 副标题写回「保留 30 天」均红）。

## 2026-11-26 第二轮：交互能力补齐

上一轮修完「假数据/窄屏/结构」后，本轮补齐**旧页有、v2 缺**的交互能力，并修掉两个
「看起来能用、实际不生效」的缺陷。

| # | 能力/缺陷 | 说明 |
| --- | --- | --- |
| 1 | **列表分页**（缺陷） | 前端固定 `limit=200` 且**忽略响应 total** ⇒ 目录 >200 项被静默截断，用户以为文件丢了。改为读 `total/offset/limit` + 「加载更多」+ 计数「已加载 X / 共 Y」；无 total 的旧搜索契约按 `count == limit` 推断。 |
| 2 | **列头排序**（缺失） | 服务端早已支持 `sort=name|size|time` + `order=asc|desc`，前端从未传。改：可排序表头（点击/Enter/Space）、同列翻转、换列重置、`aria-sort`、箭头指示。 |
| 3 | **拖拽上传**（缺失） | 旧页有 `initDragAndDrop`，v2 无。改：新增 `web/static/v2/drop-upload.js`（纯函数 `acceptsDrag`/`bump`/`hintText` + DOM 接线），提示层显示目标目录与卷；`dragenter/dragleave` 用计数抵消子元素抖动且**不得为负**。 |
| 4 | **预览相邻切换**（缺失） | 看图/看视频必须退出再点下一个。改：`←`/`→` 与头部按钮切换**同目录相邻文件**（跳过目录），显示 `i / n`，边界禁用；文本输入框聚焦时不拦截方向键。 |
| 5 | **i18n 抹掉表头子元素**（缺陷） | `applyTexts()` 直接 `n.textContent = ...` ⇒ 把 `<span class="sort-mark">` / 复选框**整个删除**，切一次语言排序箭头永久消失。改为**只改文本节点**（`textOf`/`setText`），子元素由各自逻辑维护。 |

### 测试与变异

- node：`files.test.js` 增 `parseListMeta`/`nextSort`/`sortMark`/`neighborFile`/`listURL`；
  新增 `drop-upload.test.js`（3 例）。均登记进 `make web-test`（含 `node --test`，R10 门禁）。
- e2e（`TestV2Shell`，新增播种 210 文件目录）：分页首屏 200 + 提示 + 追加到 210 + 提示消失；
  排序真实请求带 `sort=size&order=asc` 且二次点击翻 `desc`；`DragEvent` 真实拖拽后文件出现；
  `←/→` 切换预览标题。5 处变异全部命中（跳过追加 / 不传 sort / drop 不处理 / ←→ 失效 / i18n 覆写）。

## 2026-11-26 第三轮：回收站（保留期真实性 + 逐条操作）

| # | 问题/能力 | 说明 |
| --- | --- | --- |
| 1 | **保留期文案是假数据** | v2 写死「删除的文件会保留 30 天」，而 `trash.ttl` 默认 **7 天**（`trashGCTask` 里 `ttl<=0` 回落 7 天）——文案与服务端行为矛盾。改：`GET /api/config` 新增 `trash_retention`（**实际生效值**，`config.EffectiveTrashRetention` 单一实现，GC 任务同源），UI 用 `formatRetention()` 显示「超过 7 天后自动清理」；读不到 → 通用文案，**不编造天数**。 |
| 2 | **无按卷筛选** | 旧页有「卷」下拉 + 按卷清空；v2 只显示卷名不能筛。改：`volumeOptions`/`filterByVolume` 纯函数 + `#v2-trash-volume`（单卷时自动隐藏），选择持久化，计数显示「已筛 / 总数（卷 x）」。 |
| 3 | **无单条彻底删除** | 只能「恢复」或「清空全部」——想删一条得先清空整个回收站。改：新增 `DELETE /api/trash?file=&volume=`（`files.Service.PurgeTrashEntry`）**原地**删除主文件 + 同代 meta sidecar + 按原 rel 释放配额，审计 `trash_purge`；前端每行「彻底删除」（二次确认）。 |

为什么服务端新增端点而不是「恢复 + 硬删」两步：两步会把文件暴露回 `user/` 原路径
（短暂可见、可与并发上传竞争），第二步失败还会留下「已恢复但没删掉」的中间态。

### 门禁登记
R38 `route_classes.tsv`（DELETE /api/trash × 2 mux）、R35-b `mutating_route_audit.tsv`
（`action=trash_purge` + `trash_purge_test.go` 场景证据）、R55 `docs/api.md`（新端点 +
`trash_retention` 字段）。

### 验证与变异
- Go：`TestEffectiveTrashRetention`（0/负值⇒7天；显式值原样）、`TestTrashRetentionObservableOverConfigAPI`
  （默认 168h0m0s / 显式 36h0m0s）、`TestTrashPurgeEndpoint`（真删 / 保留其它条目 / 缺 file 400 /
  未知 404 / 审计 trash_purge）。
- node：`trash.test.js` 增 `volumeOptions` / `filterByVolume` / `formatRetention`（坏值返空）。
- e2e：回收站提示含「7 天」且不含「30 天」；点「彻底删除」后该条消失、其它条目保留。
- 变异 2/2 命中：提示写死「保留 30 天」⇒ 红；purge 不调服务端 ⇒ 红。

## 2026-11-26 第四轮：传输页分页（云任务 / 任务组）

| # | 问题 | 说明 |
| --- | --- | --- |
| 1 | **云任务静默截断** | 传输页固定 `GET /api/cloud/tasks?limit=100` 且**忽略 `total`** ⇒ 第 101 个任务起不可见。 |
| 2 | **筛选计数是假数据** | 四个 chip（全部/进行中/失败/已完成）的计数来自**已加载子集**：有 300 个失败任务时页面显示「失败 0+」甚至「失败 0」。且筛选只在本地做，服务端 `status=` 参数从未使用。 |
| 3 | **任务组静默截断** | `GET /api/cloud/groups?limit=50` 同样忽略 `total`（服务端本来就返回 `{groups, total}`）。 |

修复（新增 `web/static/v2/paging.js`，纯函数 + node 单测）：

- `pageMeta(body, key, loadedBefore)` → `{pageCount, loaded, total, hasTotal, hasMore}`；
  `total` 缺省/`null`/`''` 一律视为**未返回**（`Number(null)===0` 的坑），不编造总数；
- `pageURL(path, offset, limit)`：保留既有查询串，只追加分页参数（`/x?` 结尾也能正确拼接）；
- `loadedText(meta)` → 「已加载 120 / 共 300 条」；无 total 时只说「已加载 N 条」；
- `partial(meta)` → 未全部加载时四个 chip 的计数**必须带 `+`**，否则就是新的假数据；
- 状态筛选**下推服务端**（`status=failed|completed`；`running` 是非终态合集，服务端无此状态 ⇒ 仍本地筛
  并在 `partial` 时标注）；筛选变化重新拉第一页；
- 两张卡片各加「加载更多任务 / 加载更多组」+「已加载 / 共 N」文案。

### 坑（会再犯）

`paging.js` 与 `drop-upload.js` 原先放在视图脚本**之后**，而 `transfer.js` 在模块求值期就
`var pg = window.v2paging` ⇒ `Cannot read properties of undefined (reading 'pageURL')`，
整个传输页不初始化（表现为「任务列表未渲染」，无任何可见报错）。已在 HTML 中把两者移到
`shared/creds.js` 之后、所有视图脚本之前，并加注释说明原因。

### 验证与变异

- node：`paging.test.js` 6 例（total 权威 / 末页 / 无 total 不臆测 / `total:0` 有效 /
  `inferHasMore` / `pageURL` 拼接 / `loadedText` / `partial`）。
- e2e：任务列表请求含 `limit=100`；点「失败」chip 后请求含 `status=failed`；
  「已加载」文案非空。
- 变异 2/2 命中：不传分页参数 ⇒ 红；状态不下推 ⇒ 红（断言看请求 URL）。

## 2026-11-26 第五轮：用户页面配置扩展到三个功能类别

`GET/PUT /api/ui/prefs` 从「只有 delete」扩到**按功能类别**组织（同一 schema、同一存储文件
`<租户根>/meta/ui/prefs.json`）：

| 类别 | 字段 | 取值 | 安全默认 |
| --- | --- | --- | --- |
| `delete` | `default_mode` | `soft` / `hard` | `soft`（回收站可恢复） |
| `files` | `default_sort` | `name` / `size` / `time` | `name` |
| `files` | `default_order` | `asc` / `desc` | `asc` |
| `uploads` | `default_volume` | 单段卷名 / `""`=自动 | `""`（服务端按卷策略选） |

### 语义要点

- **PUT 是部分更新**：只改请求体里出现的字段；一个都没出现 ⇒ 400。
- **任何字段非法 ⇒ 400 且整请求不落盘**（不会"改了一半"）。
- `uploads.default_volume` 会被拼进 `/api/files?volume=` 与上传路由，属**外部输入**：
  含 `/`、`\`、`..`、空字节一律 400（`normalizeVolumeName` fail-closed 到空 = 自动）。
- 上传目标卷优先级：**卷过滤有值 > 用户配置的默认卷 > 空（自动）**。配置卷不存在时由服务端
  报错，**不静默改道**（避免传错卷）。
- 文件页排序优先级：**用户点列头 > 配置的默认排序 > 代码默认 name/asc**；
  设置面板保存后调 `v2filesView.reloadPrefs()` 再 `load()`（否则仍用旧 `state.sort`）。

### 前端

设置面板改为 4 个控件（删除方式 / 排序字段 / 排序方向 / 默认上传卷），**一个保存按钮**提交
三类；`prefsDirty` 覆盖全部控件（异步回填不得覆盖用户刚改的值）。

### 验证与变异

- Go：`TestUIPrefs_FilesAndUploadsCategories`（未配置默认 / 两类分别设置 / **部分更新不重置他字段** /
  清空卷 / 6 种非法值 400 且配置不变 / 多类别一次提交 / 审计 detail 记录实际改动字段）、
  `TestUIPrefs_NormalizeVolumeName`（`a/b`、`a`、`..`、`a..b`、空字节一律空）、
  `TestUIPrefs_NormalizeAllCategories`（坏配置文件逐字段归一）。
- e2e：设置里设 `size/desc` → 保存 → `/api/ui/prefs` 回读一致 → 文件页表头箭头为「大小 ↓」。
- 变异 2/2 命中：忽略配置排序 ⇒ 红（无箭头）；`normalizeVolumeName` 不校验 ⇒ 红（3 例）。

### 坑

`updateUIPrefsHandler` 三个类别堆在一个函数里 gocognit 17 > 15 ⇒ 拆成
`applyDeleteCategory` / `applyFilesCategory` / `applyUploadsCategory`（各返回
`(next, details, status)`，并共用 `categoryErrMsg` 映射文案）。
