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

## 2026-11-26 第六轮：上传目标与排序偏好（推翻上一轮的过度设计）

上一轮（已关闭的 #1299）把「排序默认值」和「默认上传卷」做成了服务端页面配置。
实测复核后**两者都撤掉**，改为更直接的设计。

### ① 排序默认配置 → 不需要（改为本地记忆）

用户质疑：*「考虑支持在 web ui 文件列表的 th 上支持点击切换排序，是否有必要专门的默认配置」*

- 列头点击排序**早已实现**（#1291）；
- 排序是**瞬时视图偏好**（我上次点了哪列），不是策略。做成服务端配置会引入
  「点列头 > 配置 > 代码默认」三层优先级，用户还得知道有这个设置存在。

**改为**：`sproxy_v2_sort` 写 localStorage（与卷过滤 / 按卷记忆子目录同款），
`normalizeSort()` 归一非法值；刷新后保持上次排序。

### ② 全部卷上传 → 真缺陷（改为禁用 + 单卷自动选中）

用户质疑：*「直接上传到当前所在卷所在目录是否就行了，必须要是具体的卷，不能在特殊聚合的
【全部卷】上进行上传，是否有必要管理上传默认卷」*

证据（都是真实代码）：

```js
// 「全部卷」⇒ state.volume === '' ⇒ 无 volume 参数
if (picked) { return picked; }
return view().defaultUploadVolume();       // 靠隐藏配置兜底
```

```go
// 服务端：无显式卷 ⇒ 自动路由（按 placement 依序试，可能落任意卷）
if explicitVol != "" { return h.routeUploadSingle(...) }
return h.routeUploadAuto(...)
```

```go
// 而「全部卷 + 某子目录」的列表是**多卷聚合**
for _, v := range vols { allFiles = append(allFiles, s.listVolumeDirEntries(...)) }
```

⇒ 用户在聚合视图的 `jav/X/` 下上传，文件落到某一张卷（不可预期），切卷过滤后就"消失"了。

**改为**：
- `canUploadTo(volume)`（纯函数）：必须非空卷名；
- 「全部卷」时上传按钮 `aria-disabled` + 视觉禁用 + `title` 说明原因，input 同时 `disabled`；
- 拖拽也不显示投放层（不承诺做不到的动作），drop 时直接提示先选卷；
- `uploadFiles()` 兜底再挡一次（拖拽/脚本/旧快捷键都可能绕过按钮状态）；
- **单卷部署自动选中该卷**（最常见场景，否则单卷用户会突然无法上传）。

`uploads.default_volume` 配置随之删除——「上传到当前卷」是唯一正确语义，配置只会让
「传到哪」更不可见。

### ③ 顺带修：「按卷记忆子目录」抢占了显式导航

`load()` 里只要 `state.volume` 有值且 hash 无子路径，就套用「该卷上次子目录」——
用户点面包屑回「全部卷」会被弹回上次目录（看起来像根目录点不进去）。改为**只在首次加载
或切卷时恢复**（`state.booted` / `__v2volSwitch`），显式导航一律以 hash 为准。

### 验证与变异

- node：`normalizeSort`（4 组非法值）+ `canUploadTo`（`''`/空格/null/undefined 全 false）。
- e2e：单卷自动选中；多卷「全部卷」上传禁用、选卷后恢复；点「大小」列 → **刷新页面** →
  箭头仍为「↑」（localStorage 记忆）。
- 变异 2/2 命中：不写 localStorage ⇒ 刷新后排序丢失（红）；`canUploadTo` 恒 true ⇒ 4 例红。

### 教训

**「加一个配置项」不总是解法**。排序、上传目标这类「当前上下文已经决定」的行为，
正确做法是让上下文自洽（上传到当前卷 / 记住上次点击），而不是新增一个用户要去发现、
维护、且与服务端策略相互覆盖的配置。页面配置只该装**真正的策略偏好**（如"删除是否硬删"）。

## 2026-11-26 第七轮：新建目录的卷归属（与上传同一条纪律）

上一轮修了「上传必须落具体卷」，留下 `POST /mkdir` 同一类问题。

### 缺陷

前端从不传 `volume`：

```js
run(api.json('POST', '/mkdir?dirname=' + enc(rel)), ...)   // 无 volume
```

服务端 `MakeDir(volName="")` 的既有语义（`TestMkdir_NoVolumeLandsOnFirstViewVolume` 锁定）：

> 实际落盘目录选 owner 视图内首个卷（默认卷优先）

⇒ 多卷环境里用户在**卷 B** 建目录，目录落到**默认卷 A**，刷新后看不到（切卷才发现建错地方）。

### 修复

1. **前端传 volume**：`/mkdir?dirname=<rel>&volume=<当前卷>`；
2. **聚合视图禁用建目录**（与上传同一条门禁）：`syncUploadGate()` 同时管
   `#v2-upload-input`（含 label）与 `#v2-mkdir`——`aria-disabled` + `.is-disabled` + `title` 说明；
3. `mkdir()` 内再兜底挡一次（脚本/快捷键可能绕过按钮状态）。

### 服务端语义**不变**（只加测试锁定）

| 场景 | 行为 | 锁定用例 |
| --- | --- | --- |
| `?volume=disk2` | 只落在 disk2 | `TestMkdir_ExplicitVolumeLandsOnThatVolume` |
| 未知卷 | fail-closed 报错，**不落默认卷** | `TestMkdir_UnknownVolumeFails` |
| 不传 volume | 落首个视图卷（默认卷优先） | `TestMkdir_NoVolumeLandsOnFirstViewVolume` |
| 建完的可见性 | 只在所属卷的列表里出现 | `TestListDir_VolumeFilterSeesMkdir` |

最后一条是**用户视角的因果**：单纯断言"文件在磁盘上"不足，必须断言"当前卷能看到、另一卷看不到"。

### 验证与变异

- Go：上述 4 条（多卷夹具 `twoVolumeHandlers`，main + disk2）；
- e2e：`/mkdir` 请求 URL 必须含 `volume=`；建目录按钮门禁与选卷状态一致；
- 变异命中：把 `mkdir` 的 `&volume=` 去掉 ⇒ `建目录请求未带 volume（会落到默认卷…）` 红。

### 关于 `/rmdir`（实测后确认**不改**）

最初以为 `/rmdir` 不带 `?volume=` 会「跨卷多删」是缺陷，实测 + 读码后确认是**刻意的逻辑
目录语义**（`pkg/files/write_ops.go` `RemoveDir` 注释）：

> 同一子目录可能因换卷在多个卷上都存在（main/disk2 各有其文件），故按 owner 视图逐卷定位、
> 对**每个存在该目录的卷**删除其子树

删「目录 X」就应把 X 在所有卷的副本删干净，否则留孤儿目录、切换卷后又看到"已删除"的目录。
而且**显式 `?volume=` 对本地卷不改变该语义**（只有外部/加密卷走 `removeDirExplicitVolume`）。

⇒ UI 删目录**不传 volume**（传了也不会变窄，反而暗示"只删一卷"）。加了
`TestRmdir_LocalVolumeDeletesLogicalDir` 把这个"看起来像 bug 的设计"钉住，防止被
"优化"成只删一卷。**本轮一度按错误假设改了 UI，靠该测试的红灯发现并回退。**

## 2026-11-27 修 CI 偶发：secretdata 窗口缓存「明文当密文」双重解密

CI 偶发（`Test(ubuntu,+Vault)` 两次：#1238、#1303）：

```
ReadAll(0): secretdata: 范围读取失败（无 parity 可恢复，不回落整块）:
  blocklet 段 0 越出合并区间（rel=262 enc=262172 span=262144）
```

`enc=262172` = 256KiB + GCM 开销，`span=262144` = 256KiB。**是明文被当密文切段**。

### 根因（契约混淆，不是算错）

```go
// 窗口缓存命中的返回是**已解密明文**
func (s *SecretdataFS) windowCacheRead(...) ([]byte, bool) {
    blobs, ok := s.stageDL.readWindowsReadyOnly(...)
    return decryptWinBlobs(blobs, o, offset, end), true   // ← 明文
}

// 但 segmentSpanFetch 用**同一个返回位**表达两种语义：
span, spanOff, ok, err := s.segmentSpanFetch(...)   // 命中时 span = 明文，spanOff = 0
out, derr := decryptSegmentWindows(o, span, spanOff, offset, end, targets)  // ← 又把明文当密文
```

`decryptSegmentWindows` 按 `bl.EncSize` 切段 ⇒ `rel(262) + 262172 > len(明文)=262144` ⇒
「越出合并区间」。

### 为什么是偶发

`stageChunkRead`（整块 staging，**先**调用）用 `readWindows` **有界等待**（10s，超时回源）；
超时返回 false 后走到 `segmentSpanFetch`，而窗口若在此刻刚就绪 ⇒ `readWindowsReadyOnly` 命中
⇒ 必炸。⇒ 只在「等待恰好超时 + 窗口随后就绪」的时序下命中，本地 `-count=30` 全绿、CI 高并行偶发。

### 修复：用结构体把两种语义分开（编译期防混用）

```go
type spanFetchResult struct {
    hit     bool    // true ⇒ plain 是已解密内容
    plain   []byte
    span    []byte  // hit=false ⇒ 密文区间
    spanOff int64
}
```

调用方：`if fr.hit { return fr.plain, true, nil }`，否则才走 `decryptSegmentWindows`。
顺带：命中但 `decryptWinBlobs` 返回 nil（窗口被篡改/与 meta 不符）⇒ 视为未命中继续回源
（此前会把 nil 当密文 span 直接报错）。

### 验证与变异

- 复现用例 `TestSegmentSpanFetch_WindowHit_NotFedToCiphertextPath`：**复刻调用方序列**
  （`segmentSpanFetch` → 按契约消费），红灯时输出与 CI 完全相同的报错；
- 契约用例 `TestSegmentSpanFetch_WindowHitReturnsPlaintext`：命中必须回明文、长度 = 请求长度；
- 端到端 `TestReadChunkRange_WindowReadyNotDoubleDecrypted`：窗口就绪后二轮读内容正确；
- 变异命中：把命中分支改回 `spanFetchResult{span: out}` ⇒ 三个断言全红；
- `go test ./pkg/volume/secretdata -race -count=2` 全绿；全包非 -race 亦绿。

### 教训

**同一个返回位不要表达两种语义**。"已解密明文" 与 "密文区间" 是两种东西，用 `[]byte` + 一个
隐含约定就必然有人（包括自己）接错。宁可多一个结构体字段，也不要靠注释约定。

## 2026-11-27 修 Windows 句柄泄漏（`os.Root` 无 finalizer ⇒ TempDir 清理失败）

CI `Test(windows)` 偶发（#1306）：

```
--- FAIL: TestEncReadCloserSeek_UsesBlockSkip
    testing.go:1619: TempDir RemoveAll cleanup: unlinkat
      C:\Users\RUNNER~1\AppData\Local\Temp\TestEncReadCloserSeek_UsesBlockSkip\001:
      The process cannot access the file because it is being used by another process.
```

### 根因：`os.Root` 持有目录句柄且**没有 finalizer**

`storage.OpenRoot` 包的是 `os.OpenRoot(path)`——它持有一个**打开的目录 fd**。
`pkg/storage.Root` 早有 `Close()`，但**测试里 30+ 个调用点大多没调**：
不关就一直持有到进程退出 ⇒ Windows 上 `t.TempDir()` 的 `RemoveAll` 删除该目录时被占用。

（Linux 上删除"被打开的文件/目录"通常仍成功，所以只在 Windows 暴露。）

### 修复

1. **测试助手 `openTestRoot(t, dir)`**（`pkg/storage/testroot_helpers_test.go`）：
   打开 + `t.Cleanup(Close)`。把 31 处同构调用点批量改为走它；
   顺手把 `root_test.go` 里一处「打开成功才 Fatal」的泄漏点补上 Close。
2. **合并已有两个同名助手**（`pkg/storage` 的无参 `openTestRoot(t)` 与
   `pkg/storage/capacity` 的参数版实为两份实现），`pkg/storage` 只留参数版一个真身；
   capacity 包因跨包（`*storage.Root`）保留自己的薄封装。
3. **`Root.Close()` 幂等 + nil 安全**（重复 Close 返回 nil，`errors.Is(os.ErrClosed)` 吸收）。

### 一个必须记录的坑：不要置空内部指针

我最初写了 `rt.r = nil` 以求幂等，结果 `pkg/files` 整包 **panic**：

```
panic: runtime error: invalid memory address or nil pointer dereference
os.doInRoot(0x0, ...) <- os.(*Root).MkdirAll <- checksum.(*ChecksumStore).save
  <- savemerger.flushOnce <- time.goFunc
```

—— 后台 debounce goroutine 在测试清理**之后**仍会写一次；把 `rt.r` 置空把
「关闭后再用」从 `os.ErrClosed`（干净错误）变成 **nil 指针 panic**。
⇒ 只吸收 `ErrClosed`，**不动指针**。

### 门禁：`TestOpenRoot_CallSitesRegisterClose`

源码级扫描 `pkg/storage/*_test.go` 的每个 `OpenRoot(` 调用点：要么走 `openTestRoot(...)`，
要么其后 8 行内出现 `.Close()` / `t.Cleanup(...Close...)`。含**下限护栏**（扫到 0 个即失败，
防解析器失效导致假绿）。

上线即抓到一处真实漏洞（`root_test.go:91` 的 `if _, err := OpenRoot(dir); err == nil`）。

### 验证与变异

- `go test ./pkg/storage/... -race -count=1`、`./pkg/storage -race -count=2` 全绿；
- `go test ./pkg/files ./pkg/server ./internal/archcheck` 全绿（确认没把 panic 带出去）；
- 变异命中：把某处 `openTestRoot(t, dir)` 换成裸 `OpenRoot` ⇒ 门禁报
  「未登记关闭（os.Root 无 finalizer…）」。

### 边界

`pkg/storage` **之外**的测试（`pkg/files` / `pkg/server` / `pkg/checksum` …）同样有
`OpenRoot` 调用点，理论上也会泄漏句柄。本轮只修 `pkg/storage`（CI 实际命中处）+
提供助手与门禁范式；其余包可按同一模式逐步迁移（需要时把门禁扩到全仓）。

### 追加：同批修掉「debounce 落盘协程」类的 TempDir 清理 flake

`Root.Close()` 修好后，同一 PR 的 CI 露出**下一个** Windows 清理失败（原因不同）：

```
--- FAIL: TestTrash_RestoreRestoresChecksumLedger
    TempDir RemoveAll cleanup: unlinkat ...\alice\meta: The directory is not empty.
```

根因：`checksum.ChecksumStore` 带 **debounce 落盘协程**（`internal/savemerger`）。
测试创建它但**从不 Close**（31 处调用点全部如此）⇒ 协程在测试结束后仍写一次
⇒ 与 `t.TempDir()` 的 `RemoveAll` 撞车（`RemoveAll` 已遍历完 meta 目录、随后又冒出文件）。

修复（同一 flake 类必须一起修，否则换个用例又炸）：
- `pkg/checksum` 6 处调用点补 `t.Cleanup(func(){ _ = cs.Close() })`；
- `pkg/files` 三个夹具（`dirsEnv` / `externalChunkEnv` / `chunkedTestEnv`）在 `t.Cleanup`
  里**先停协程、再关 root 句柄**（LIFO 注册顺序）；
- `pkg/cloud` 夹具同理（遍历已建 store 逐个 Close）；
- `pkg/server`（`integration_test` / `store_test` / `e2e_test` / `verify`）与
  `pkg/storage/capacity` 逐处补；
- 新增门禁 `pkg/checksum/TestNewChecksumStore_CallSitesRegisterClose`（源码级 + 下限护栏）。
  上线即抓到 `pkg/checksum` 自身 6 处遗漏。

变异命中：删掉任一处 `t.Cleanup(... cs.Close())` ⇒ 门禁报「未登记关闭」。

**教训**：修「测试夹具没释放资源」时，**同一类资源（协程 / 句柄 / 监听器）必须一次清干净**，
否则修完一个 flake 只是让下一个浮出水面 —— 而每轮 CI 都是十分钟级成本。

## 2026-11-27 门禁 R60：把「夹具释放资源」从局部纪律升级为全仓门禁

上一轮只守 `pkg/storage` 与 `pkg/checksum`；本轮把同类违规**在全仓一次清干净**并收口为门禁。

### 清理（30 处 `OpenRoot` + 3 处 checksum store）

| 包 | 补 `t.Cleanup(Close)` |
| --- | --- |
| `cmd/sproxy` | 9（`secret_register_test` 8 / `user_volume_restore_test` 1→2 处） |
| `pkg/server` | 10（`chunked_owner` / `chunked_upload` / `cloud_download_handler`×3 / `dav` / `download_external` / `files_external_ops` / `handlers` / `stream_followup` / `upload_init` / `upload_owner`） |
| `pkg/files` | 2（`dirs_test` / `transform_cache_gc_test`） |
| `pkg/sync` / `pkg/volume/registry` | 3 |

`pkg/files/dirs_test.go` 与 `pkg/cloud` 的 checksum store 属**容器统一关闭**（`dirsEnv` /
`newCloudTestEnv` 的 `t.Cleanup` 遍历关闭），在例外清单登记并写理由 —— 避免"两处都登记"的重复。

### 门禁 R60（`internal/archcheck/test_resource_release_gate_test.go`）

- 扫描全仓 `*_test.go`（跳过 `build/` `node_modules/` `.git/` `.worktrees/` `vendor/`）；
- 判据：`storage.OpenRoot(` / `checksum.NewChecksumStore(` 调用点后 10 行内必须有
  `Close()` 或 `t.Cleanup(`；助手（`openTestRoot`/`openRootForTest`/`testRootAt`）与定义行不计；
- 例外清单 `internal/archcheck/test_resource_exceptions.tsv`（`<rel>:<line>\t<理由>`，**只减不增**）；
- **双侧下限护栏**：测试文件 ≥900、调用点 ≥130（实测 142），防扫描面被改窄或正则失效假绿；
- 跳过两个「门禁自身」文件——它们的匹配字符串不是真实调用（否则门禁会举报自己）。

### 关键实现细节（复用价值）

**跳过目录必须用「相对 module root」的路径判定**：worktree 的绝对路径本身含
`/.worktrees/`，用绝对路径判定会把**整个仓库**跳过（扫描 0 个 → 靠下限护栏才发现）。
这是 R57 已踩过的同一个坑。

### 验证与变异

- `go test ./internal/archcheck`（含 R60 + R56 索引门禁）全绿；
- 受影响 8 个包 + `cmd/sproxy` 全绿；`golangci-lint ./...` 0 issues；
- 变异命中：删掉 `pkg/server/stream_followup_test.go` 的 `t.Cleanup(... root.Close())`
  ⇒ R60 报 `pkg/server/stream_followup_test.go:39 …`（证明门禁确实覆盖到**别的包**）。
- 索引登记：`docs/gates.md` 增补 R60 行（R56 门禁强制）。

### 关于「门禁只能减少不能新增」的裁定

本门禁属**收口既有纪律**（上一轮已用同样的判据守了两个包），不是提高既有门禁基线：
它把「测试夹具必须释放资源」这条已在 AGENTS 与设计文档里写明的纪律变成可执行判据，
且**新代码不会因为「以前没人管」而被卡** —— 清完 30 处违规后才上线，起点是零违规。

### 追加：`OpenTenant` 产生的**租户根**是第三层句柄

R60 上线后 CI 立刻抓到同一个 flake 的**另一种形态**（`pkg/server/s3`）：

```
--- FAIL: TestListObjectsV2_NamedBucket
    TempDir RemoveAll cleanup: unlinkat ...\002\EXAMPLEACCESSKEY0001:
      The process cannot access the file because it is being used by another process.
```

`bucketRoot` 本身**关了**，但 `storage.OpenTenant(bucketRoot, owner)` 会**再开一个**
`os.Root`（租户子根）—— 这也需要关闭，而 `storage.Tenant` **没有 `Close()`**。

修：
- `storage.Tenant.Close()`（幂等 + nil 安全，委托 root.Close）；
- R60 的资源模式加入 `OpenTenant(`（三类：`OpenRoot` / `OpenTenant` / `NewChecksumStore`）；
- `pkg/server/s3` 两处 `bucketTenant` 补 `t.Cleanup(Close)`；
- 例外清单补 3 条（`dirsEnv` 统一关租户根；`tenant_resolve_test` 的两处调用**必然失败**
  不产生资源；s3 两处 `t.Fatalf` 文案属误匹配）。

下限护栏提到 **145**（实测 158）。

**这已经是同一类资源在同一 PR 里第二次「换个形态露出来」** —— 印证了
「同一类资源必须一次清干净」的纪律：`os.Root` 有三个创建入口（`OpenRoot` / `OpenTenant` /
`TenantCache`），只堵一个不够。

## 2026-11-27 `pkg/cloud` 并发面审计：锁纪律良好，补行为级守卫

结论先行：**`pkg/cloud` 的锁纪律执行得很好**，除已修的 `settleArtifactsOnFail`（#1304）外，
没有发现新的锁外写。

### 审计方法与结论

用脚本枚举 `pkg/cloud` 非测试代码里对 task 字段的写入（61 处），按「所在函数是否出现
`m.mu.Lock()`」初筛 → 22 处"疑似"。逐个核对后**全部是误报**：

| 误报来源 | 例 |
| --- | --- |
| `xxxLocked` 后缀函数（约定：调用方持锁） | `rotateSourceLocked` / `forceRotateSourceLocked` / `clearRunningLocked` |
| 注释已声明「调用方持 m.mu」 | `recordProgressSample` / `recordChunkState` / `diag` / `reconcileReservedSize` / `failTaskOnStorageFull` |
| 调用点显式取锁 | `manager_query.go:210`（`m.mu.Lock(); releaseTaskScope(t)`） |

⇒ **文本判据不可靠**（22/61 误报率），放弃"扫描赋值是否在锁内"的门禁思路。

### 改为行为级守卫

新增 `pkg/cloud/manager_concurrent_readers_test.go`：真实下载进行中，**紧凑轮询**读面
（`ListTasks` / `SnapshotTask` / `GetTask` / `saveTask` / `markDirty`）与下载 goroutine 的
写面（`task.Downloaded`、`recordProgressSample`、`recordChunkState`）并发，`-race` 必须无告警。

### 让守卫真的有效（两个必须踩的坑）

1. **`SyncThreshold <= 0` 会被配置归一成默认 20MB**（`applyCloudConfigDefaults`）⇒ 下载走
   **同步**路径、在 `SubmitAndStart` 内完成 ⇒ 读面**完全无法重叠**（首版测试的
   `before wait` 与 `after wait` 时间戳相同，一眼看出）。必须显式给正值 `1`。
2. **窗口太短 ⇒ 变异不报**：512KiB × 1ms 只产生 **1** 个进度采样点。改为
   4MiB / 32KiB 分块 / 2ms 间隔 ⇒ 进度回调 **130+ 次**、下载持续 ~250ms。

这两条与 #1308 的教训同源：**并发测试必须让「被竞争的那次写入」反复发生**，
且必须确认读面真的在执行（时间戳/计数器探针，别假设它跑了）。

### 变异

| 变异 | 结果 |
| --- | --- |
| `progressFn` 里把 `task.Downloaded = downloaded` 移到 `m.mu.Lock()` **之前** | `-race` 报 4 处 DATA RACE ✅ |

验证：`go test ./pkg/cloud -race -count=2` 全绿；`go build ./...`；`golangci-lint` 0 issues；
`internal/archcheck` 全绿。

### 顺带确认（无改动）

- `pkg/downloader` 的 HTTP 下载器**确实**通过 `copyBodyWithProgress(..., pw.onProgress, ...)`
  回调进度（早期 grep 范围太窄曾误判"完全忽略"）；
- `WithChunkProgress` 只有**分块型**下载器（网盘类）会报，内建 HTTP 下载器不报
  ⇒ 进度条对 HTTP 直链只有 `Downloaded`，无分块明细 —— 属既有设计。

### 追加：`registry.Set` 是**第四层**句柄持有者（s3 flake 的真因）

R60 上线后 CI 仍报同一 flake（`TestListObjectsV2_NamedBucket` / `TestBucketRouting`）：

```
TempDir RemoveAll cleanup: unlinkat ...\002\EXAMPLEACCESSKEY0001:
  The process cannot access the file because it is being used by another process.
```

**#1316 的修复（关 `bucketTenant`）并不够** —— 本地 `-count=5` 仍**必现**。

真因：请求处理路径 `Service.TenantFor` → `registry.Set.Tenant(vol, owner, log)`：

```go
// pkg/volume/registry/set.go
vs.cacheMu.Lock()
c, ok := vs.caches[volName]
if !ok {
    c = storage.NewTenantCache(vs.roots[volName], ...)  // ← 每个卷一个 TenantCache
    vs.caches[volName] = c
}
vs.cacheMu.Unlock()
return c.TenantFor(owner)   // ← 懒建租户子根（独立 os.Root）
```

⇒ **测试为请求路径懒建的租户根没人关**。`Set.Close()` 本就会关这些缓存，只是
`s3` 的 `testVolumeSet` 从没调用它。

修：`testVolumeSet` 里 `t.Cleanup(func(){ _ = set.Close() })`（`-count=8` 全绿）。

**`os.Root` 的创建入口至此确认有四个**：`OpenRoot` / `OpenTenant` / `TenantCache.TenantFor` /
`registry.Set.Tenant`。每堵一层 CI 就换下一层露出来。

### 遗留（独立 PR）

全仓还有 **49 处** `registry.NewSet(` 未登记关闭（20 个文件：`cmd/sproxy`、
`pkg/server`、`pkg/files`、`pkg/volume/{registry,enclosure}`）。属同一 flake 类的第五轮清扫，
单独 PR 处理（并把 R60 的资源模式扩到 `registry.NewSet(`）。
本轮只修 `pkg/server/s3`（CI 实际命中处）。

## 2026-11-27 R60 扩到 `registry.NewSet`（第五轮，也是这一类的收尾）

上一轮把 `registry.Set` 认定为第四层句柄持有者，但只在 s3 修了 CI 命中处。本轮补完全仓。

### 清理

`registry.NewSet(` 的调用点分三类处理：

| 类别 | 处理 |
| --- | --- |
| 持有本地卷根 且 作用域内有 `t` | 加 `t.Cleanup(func(){ _ = set.Close() })`（**35 处**，18 个文件） |
| `roots=nil` / 空 map（`Set.Close()` 关不到任何东西） | 登记例外清单 + 写明「无句柄可关」 |
| 助手函数**没有 `t`** 参数（`volumeDimSet` / `presignVolWriteSet` / `poolWaitSet` / …） | 同上（它们都 `roots=nil`，改签名只是噪声） |

`secret_register_test.go` 的 `newSet` 闭包、`meta_remote_test.go`、`baidupcs_sync*.go`
等真实持有的站点逐个补齐。

### 门禁 R60 扩展

- 资源模式加 `registry.NewSet(`（`Set` 内部为每卷懒建 `TenantCache`，缓存着租户子根）；
- 下限护栏调用点 **145 → 190**（实测 200+）；
- **例外清单键从「行号」改为「代码行文本」**：
  ```diff
  - pkg/files/dirs_test.go:487	理由
  + pkg/files/dirs_test.go:r, err := storage.OpenRoot(e.root)	理由
  ```
  行号随每次编辑漂移 —— 实测加 2 行注释就让 3 条例外**全部失配**、门禁无故变红。
  行文本变化时例外失效，属 fail-closed 的正确方向。

### `-count>1` 的两处既有噪声（与本次无关，未改）

`pkg/volume/registry` 的 `TestLocalBackendRegistration_AllowedButNotUserCreatable` 与
`pkg/volume/enclosure` 的 `TestAttachAll_*` 在 `-count>1` 下失败：前者断言「全局 registry
不应预注册 local」，后者全局 wrapper 重复注册 panic —— 都属**全局状态 + 重复执行**的既有
限制，CI 用 `-count=1` 不受影响。

### 验证

`cmd/sproxy` / `pkg/files` / `pkg/server`（含 `s3`、`remotefs`）/ `pkg/volume/registry` /
`pkg/volume/enclosure` / `pkg/cloud` / `internal/archcheck` 全绿；`go build ./...`；
`golangci-lint ./...` 0 issues。

变异命中：删掉 s3 `testVolumeSet` 的 `set.Close()` ⇒ R60 报 `pkg/server/s3/object_test.go:39`。

### 这一类的完整入口清单（四次 CI 才收敛）

| 轮次 | 入口 | 症状 |
| --- | --- | --- |
| 1 | `ChecksumStore`（debounce 协程） | `The directory is not empty` |
| 2 | `storage.OpenRoot` | `being used by another process` |
| 3 | `storage.OpenTenant`（租户子根） | 同上 |
| 4 | `registry.Set` → `TenantCache` → `TenantFor` | 同上 |
| 5 | 全仓 `registry.NewSet(` 收口（本轮） | —— 已由 R60 钉住 |

**教训**：从第一次发现就该把所有持有 `os.Root` 的入口列全（`grep -rn "os.OpenRoot"`），
而不是每被 CI 打一次才补一层。
