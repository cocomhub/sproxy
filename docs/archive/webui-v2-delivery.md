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
