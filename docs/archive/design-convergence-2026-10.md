> **归档说明（2026-11-13，用户口径）**：本文件为**精简后的背景知识**——保留**设计意图 / 经验 /
> 原则 / 依赖与约束**，**移除具体实现**（代码块、逐行文件/函数引用、任务拆解步骤、验收清单）。
> 实现细节以**代码与 git 历史**为准（多轮迭代后可能已不一致，故不在此固化）。

# 设计收敛：历史 plans/specs 的有效结论归档（2026-10 及更早）

> 来源：`docs/superpowers/plans/`、`docs/superpowers/specs/`（按父级 AGENTS 规则 3：
> 规划文档的**有价值内容提炼后即移除**）。此处**逐字保留**各源的「设计决策 / 结论 / 教训 /
> 风险 / 已知限制」小节，供后续维护者当**背景知识**复用；过程性内容（任务拆解、逐步操作、
> 验收清单）不保留（实现已完成，见 `docs/api.md`/`docs/architecture.md`/`docs/gate-*.md` 等权威文档）。

---

## 来源（**已移除**；如需实现细节请查 git 历史 `specs/2026-09-22-stealth-builder-design.md`）：

## 7. 已知限制与决策记录

1. **import path 不混淆**：`github.com/cocomhub/sproxy/pkg/...` 在符号表与
   panic 栈中仍可能出现（garble 默认对模块路径做 hash 化，但 module path 本身
   在 go.mod 内必须保留）。**判定：可接受**——混淆目标是不被 `strings` 一眼
   识别，不是防专业逆向；goroutine dump 里的包路径不构成品牌暴露。
2. **配置路径**：`~/.config/sproxy`（XDG）随品牌表替换为 `~/.config/prism`。
   **行为影响**：隐藏二进制与旧版数据/配置**不兼容**（不同目录）。
   用户已确认「隐藏场景接受独立配置目录」。
3. **TLS 证书**：自签证书的 CN/SAN 中可能含主机名，不涉及品牌；
   若用户在证书里写了 sproxy，需自行注意。
4. **日志与遥测**：日志前缀、tracer 名随品牌表替换；日志内容中的
   业务字段（如文件名）不含品牌，无需处理。
5. **单二进制双角色**：bridge 形态 = 隐藏配置（含 server_url、凭据、操作参数）
   加密后内嵌（见 §8），启动直接执行固定操作，不提供子命令。
6. **版本输出**：隐藏二进制不再提供真实版本号（避免版本指纹）。
   排障时用 `--build` 时间戳/哈希替代，或在构建报告里留档。
7. **网络协议指纹**（HTTP 头、路由路径、默认端口）不在本规格范围，
   由用户的协议伪装工程单独处理。

---

## 来源（**已移除**；如需实现细节请查 git 历史 `specs/2026-10-06-audit-webui-design.md`）：

## 3. 设计决策
### 决策一：卷类型分层 + schema 驱动建卷表单（`/api/backends` 扩展）

**卷类型 tag（4 类）**：
| tag | 含义 | 示例 type | 是否建卷 | 底层卷 |
|-----|------|-----------|----------|--------|
| `mt-local` | 系统本地卷（config `volume[]` 声明） | local | 只读展示 | — |
| `linked` | 网盘卷（需账号凭据） | baidupcs/pikpak/s3 | ✅ | — |
| `wrapper` | 封装卷（加密/冗余/出口/密钥） | secretdata/secrets/egress | ✅ | 可选择任意真实卷 |
| `secrets` | 密钥管理卷 | secrets | ✅ | 本地/网盘/加密盘 |

- **secrets 也是 wrapper**：底层本地盘、云盘、甚至**加密盘**（secretdata 之上再套一层）——`type=secrets` 复用 store 封装，target 可选任意真实卷。
- **嵌套允许 + 防成环**：底层卷可选任意已注册卷（含 wrapper），但创建时做**闭环检测**（防 `secrets→secretdata→secrets` 循环引用），fail-closed。

**`GET /api/backends` 扩展**（`pkg/server/backends_api.go`）：
- **schema 来源**：后端实现可选 `registry.SchemaProvider` 接口（`Schema() FieldSchema`）；未实现则只给 `type`/`category`（表单只填 name）。
- **`NewBackend`** 用 schema 校验 `Extra`（封装卷必填底层卷 + 卷存在 + 防环）。
### 决策二：通用审计日志包 `pkg/audit`（核心）

**定位**：跨层级 span 式计量 + 独立文件每行落盘，**可复用**于任意场景（云下载/转存/加密/同步/上传/批处理）。

#### 4.1 核心类型 `pkg/audit/audit.go`

#### 4.2 `Save/Get`（跨层级数据粘合）
Logger 承载一个 `map[string]any`（bound 到作用域），配合 `Span.End` 供上层累计（如 transfer 把字节写入，encrypt 读取对照明密文）。

#### 4.3 Sink —— 独立文件每行（天然任务隔离 + 旋转）
`pkg/audit/store.go`：
- 复用现有 `AuditStore` 落盘模式泛化：`NewScopeSink(dir, scopeID, dim)` → `<audit_root>/<scopeID>/<dim>.audit.log`。
- `Append(Row)` 写一行 JSON；行 sync、旋转、`Recent(filter)`。
- **每任务独立文件** ⇒ 天然隔离，多任务并行互不串行。

#### 4.4 ctx 跨层（span 式）
所有阶段埋点在任何深度经 `From(ctx)` 拿同一 Logger 记 span；任务隔离靠独立文件（scopeID 派生）。

#### 4.5 归档到卷（未来扩展，接口预留 + 配置门）
- **默认不归档**；开启时需**选卷 + 路径**（config `audit.archive.enabled/volume/path`）。把 `<scope>.audit.log` 副本写入所选卷（如 secretdata://audit/）。

#### 4.6 与现有 `server/audit*.go` 关系：**并存 + 预留迁移**
- 保留现有 `AuditEvent`/`AuditStore`/`/api/audit`（敏感操作即时 ring），**零改动**。
- 新 `pkg/audit` 承载 span 计量 + 每任务文件；`TypeSensitive` 预留迁移桥，未来可合流。

---

## 来源（**已移除**；如需实现细节请查 git 历史 `plans/2026-10-06-audit-webui.md`）：（任务拆解已执行完毕；仅保留设计性约束与审查重点）

## 全局约束

- **安全红线**：双口令派生仅 CLI（浏览器无 scrypt）——Web 只做 random/import/export；`Row.Meta`/审计持久化不得含原始凭据/口令/secret 明文；审计文件权限 0600。
- **审计数据不泄卷**：无 ACL 用户列表不见 + 下载 404 fail-closed；`?volume=` 指定无权卷 → 404（不泄卷存在性）。
- **防成环**：封装卷创建时底层卷闭环检测，fail-closed。
- **三行为语义空洞**：`download_local=false` + 无 transfer + `save=false` → 服务端拒绝创建。
- **前端约定**：纯函数放 `*-format.js`（顶层 function + module.exports）；跨文件隐式全局顶部 `// global:` + typeof 守卫；新 JS 同时进 Makefile `web-test` 的 node --check 与 node --test 双清单；动态 HTML 一律 `var(--…)` 禁内联亮色 hex；暗色 token 三块（:root/@media/[data-theme]）同步。
- **Go 约定**：错误优先 `fmt.Errorf("...: %w", err)`；哨兵错误 `var ErrXxx = errors.New(...)`；新测试默认 `t.Parallel()`（无法并发者显式登记，见 sproxy CLAUDE.md R18）；禁止 `http.DefaultClient`/共享 Transport；127.0.0.1 回环绑定。
- **命名**：文件 `snake_case.go`；构造函数 `New`+类型名；类型全名与规格 §4.1 一致。
## 审查重点（Review Focus）

1. **底层卷成环**：`secretdata→secrets→secretdata` 循环。创建封装卷时目标底层卷链必须无环，成环 → 400 拒绝，不落盘。
2. **无权卷列表**：`GET /api/files?volume=<无权外部卷>` → 404，不泄卷存在性（与既有 `?volume=` ACL 行为一致）。
3. **审计不含明文 secret**：`Row.Meta` 与 `/api/audit` 响应不得出现 64hex secret / 口令 / 下载凭据；落盘 0600。
4. **播放 Range 越界**：播放器发超范围 Range → 206/416 正常处理（`ServeContent` 语义），不 panic、不整文件泄漏。
5. **三行为空洞**：任务创建 `{download_local:false, transfer:null, save:false}` → 400。
6. **审计归档默认关**：未配置 `audit.archive.*` 时绝不写归档文件；开启需选卷+路径。

---

**文件：**
- 创建：`pkg/audit/audit.go`
- 创建：`pkg/audit/audit_test.go`

（`memSink`/`mustLogger`/`mustLoggerSink`/`ByteKV`/`BandwidthKV` 为测试辅助，定义在同文件底部，实现 `Sink` 接口。）

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：编译失败，`undefined: NewScope / TypeDownload / ...`（包不存在）。

核心签名（函数体由实现者写，规格 §4.1 已定类型）：

要点：`Begin` 记录 `StartMS`（距作用域起点）；`Span.End` 填 `DurMS` 并 `Append`；`Save/Get` 存 `map[string]any`（幂等）；`With` 返回同 scope 新 Logger 但 `dim` 已换；**nil Logger 上所有方法必须 no-op 不 panic**（`From` 返回 nil 时链式调用安全）。kv 解析：`kv ...any` 按 `key,value` 对解析，`ByteKV`/`BandwidthKV` 辅助返回 `(string,any)` 对；无法成对时忽略该 kv。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：PASS（含 `-race` 默认开启）。

---

**文件：**
- 创建：`pkg/audit/store.go`
- 创建：`pkg/audit/store_test.go`

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：编译失败，`undefined: NewScopeSink`。

要点：文件 `os.CreateTemp`-free 直接 `os.OpenFile(dir/scopeID+".audit.log", O_CREATE|O_WRONLY|O_APPEND, 0600)`；`Append` 行内 JSON 用 `encoding/json.Marshal`；`Recent` 逐行 `json.Unmarshal` 到 `Row` 再按 `Filter` 精确相等过滤；`Close` 幂等。目录不存在时 `os.MkdirAll`（0700）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：PASS。

---

**文件：**
- 修改：`pkg/cloud/manager.go`（`CloudTask` 增 `Audit []audit.Row` json 字段）
- 修改：`pkg/cloud/manager_task.go`（下载阶段 Begin/End）
- 修改：`pkg/cloud/transfer.go`（转存阶段）
- 修改：`pkg/volume/secretdata/secretdata.go`（`WriteFile` 加密耗时 span）
- 修改：`pkg/server/cloud_download_handler.go`（任务详情响应含 audit）
- 测试：`pkg/cloud/audit_integration_test.go`（新建）

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：FAIL（`task.Audit` 不存在或为空）。

- `CloudTask` 增 `Audit []audit.Row json:"audit,omitempty"`（`manager.go:40-90` 结构内）。
- `manager_task.go` 下载循环：`audit.From(ctx)` 拿 Logger（nil 安全）→ `Begin("download")`→下载→`End(ByteKV(Bytes: n), BandwidthKV(...))`；任务级 `NewScope(task.ID, ScopeOpts{NodeID})`，经 `WithContext` 注入 task ctx；完成/失败时 `Flush` 且把 `rows := sink.Recent(Filter{})` 复制进 `task.Audit`（持久化随任务）。
- `transfer.go` `transferOnce`：`Begin("transfer")`→`End(ByteKV(...))`；若目标为 secretdata（FS 断言 `TypeEncrypt` 路径），`Begin("encrypt")` 在 `WriteFile` 外包计时 + 明文/密文对比字节。
- `cloud_download_handler.go` 任务详情响应已含 `CloudTask` JSON → 自动带 `audit` 字段（无需改响应结构，验证即可）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：PASS。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/cloud/`
预期：PASS（既有任务无回归）。

---

**文件：**
- 修改：`pkg/volume/secretdata/secretdata.go`（`WriteFile` 外包 audit span；`OpenRangeRead` 内 `decryptRangeBlocklet` 计时）
- 修改：`pkg/cloud/audit_integration_test.go`（加密卷转存断言补全）

在 `TestCloudTask_Audit_Collected` 中增：`transfer` 到 secretdata 卷 → 断言存在 `TypeEncrypt` 行且 `Bytes>0`（明文体积）、`Meta` 含 `cipher_bytes`。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：FAIL（无 encrypt 行）。

`secretdata.go` 的 `WriteFile`（`:555`）外包：`From(ctx).Begin("encrypt")` → 写后 `End(ByteKV(Bytes: size), KV("cipher_bytes", 已写密文字节))`；`decryptRangeBlocklet`（`:539`）同 span。加密算法/档位放入 `Meta`（`algorithm`）。nil Logger 安全。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/ && go test -count=1 ./pkg/volume/secretdata/`
预期：PASS。

---

**文件：**
- 修改：`pkg/volume/registry/backend.go`（新增接口 + `BackendSchemas()`）
- 修改：`pkg/server/backends_api.go`（响应结构 + 组装）
- 测试：`pkg/server/backends_api_test.go`（新建）；`pkg/volume/registry/backend_test.go`（增）

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/server/ -run TestBackendsAPI_SchemaDriven ./pkg/volume/registry/ -run TestBackendSchemas_Provider_Only`
预期：FAIL（`BackendSchemas` undefined / 响应无 category）。

`pkg/volume/registry/backend.go` 增：

`pkg/server/backends_api.go` 的 `backendsHandler` 改返回 `BackendSchemas()`（替代 `[]string`）；`Label` 留空缺省（前端按 type 显示）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/server/ -run TestBackendsAPI_SchemaDriven && go test -count=1 ./pkg/volume/registry/`
预期：PASS。

---

**文件：**
- 修改：`pkg/server/user_volume_api.go`（`createUserVolumeHandler` 加 schema 校验 + 防环）
- 修改：`pkg/server/user_volume_api_test.go`（增用例）

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestCreateUserVolume' ./pkg/server/`
预期：FAIL（缺 target 仍成功 / 环仍成功）。

`createUserVolumeHandler` 在 `registry.NewBackend` 前增加：
1. `typ` 的 schema 中 `volume-select` 必填字段逐一校验 `Extra[key]` 非空；
2. `AllowWrapper=false` 的 volume-select 目标必须是非 wrapper 卷；`AllowWrapper=true` 允许 wrapper；
3. **防环**：沿 `target` 链向上遍历（`volSet.External(name)` 的 `Extra["target"]` 递归），若回落到自身 → 400 `errVolumeCycle`；
4. 复用既有 `UserVolume` store（Create 重名 409 / Delete 引用 409 保留）。

新增哨兵错误：`var errVolumeCycle = errors.New("volume: 底层卷成环引用")`（`user_volume_api.go`）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestCreateUserVolume' ./pkg/server/`
预期：PASS。

---

**文件：**
- 修改：`pkg/files/read_ops.go`（`List` 增外部卷分支）
- 修改：`pkg/server/files_api_test.go` 或 `pkg/files/read_test.go`（增用例）
- 测试：`pkg/files/external_list_test.go`（新建）

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestList_External' ./pkg/files/`
预期：FAIL（外部卷返回空）。

`read_ops.go` 的 `List`（`:92-148`）：当 `opts.Volume != ""` 且该卷是 `registry.Set.External(name)`（外部后端）时，改走 `be.FS().ListDir(ctx, subdir)` → 把 `[]syncpkg.Entry` 转 `[]FileInfo`（`IsDir`、`Name`、`Size`、无 checksum/无 volume 字段）返回；本地卷路径零改动。`listFallback` 对未知/无权卷保持 404（不泄存在性）。文件模型 `FileInfo`（`read.go:39-49`）复用。

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestList_External' ./pkg/files/ && go test -count=1 ./pkg/server/ -run 'TestFiles|TestList'`
预期：PASS。

---

**文件：**
- 修改：`web/static/sclient/api/cloud.js`（`createDownload/createBatch/createGroup` 透传）
- 修改：`web/static/app.js`（`doSubmitCloudTasks` 传 transfer/save/download_local）
- 修改：`web/static/app-render.js`（云下载表单含目标卷选择器/路径/三行为复选框）
- 修改：`web/static/app-render.test.js`（纯函数断言）
- 修改：`Makefile`（`web-test` 已覆盖 app-render，无需新文件；cloud.js 已在清单）

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js`
预期：FAIL（`cloudDownloadFormHtml` undefined）。

- `cloud.js`：`createDownload(url, filename, opts)`、`createBatch(urls, opts)`、`createGroup(name, urls, opts)` 三方法把 `opts.transfer`（`{volume,path}`）、`opts.save`、`opts.downloadLocal` 并入请求体（对齐后端 `TransferSpec`/`TaskParams`，三行为正交、缺省 undefined 不发送 → 零回归）。
- `app-render.js`：新增纯函数 `cloudDownloadFormHtml({volumes, current})`——目标卷下拉（`name="transfer-volume"`，选项 `local` 等 + 空=不转存）+ 目标路径输入 + 「保留服务端副本(save)」「下载到本地(download_local)」复选框。
- `app.js` `doSubmitCloudTasks`（`:2474`）：读表单字段 → 构造 `{transfer:{volume,path}, save, download_local}` 传 `sc.cloud.createDownload/createBatch`；`doChainDownloadCloud`/`doChainDownloadCloudGroup` 同步透传。
- 空白页/未选卷：transfer 字段为 `undefined`（后端空语义 = 不转存，零回归）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js && node --check web/static/sclient/api/cloud.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

---

**文件：**
- 修改：`web/static/index.html`（文件页工具栏加「卷」下拉）
- 修改：`web/static/app.js`（卷下拉填充/切换 → refreshList 带 volume）
- 修改：`web/static/app-render.js`（`buildFileRowHtml` vol-badge 加 category 类）
- 修改：`web/static/style.css`（`--badge-wrapper-*` token 三块同步）
- 修改：`web/static/app-render.test.js`

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js`
预期：FAIL（`volume_category` 未消费）。

- `GET /api/files?volume=` 响应 `FileInfo` 增 `VolumeCategory string json:"volume_category,omitempty"`（`read.go:39-49`，任务 7 已透传外部卷，此字段补分类）。
- `app-render.js buildFileRowHtml`：`fi.volume_category === 'wrapper'` → badge 加 `vol-badge-wrapper` 类 + 🔒图标。
- `index.html`：文件页工具栏加 `<select id="volume-filter">`（首项「全部卷」）。
- `app.js`：`initVolumeFilter` 填充（`GET /api/volumes` 全部可见卷含加密卷）→ change 调 `refreshList({volume})`；`refreshList` 带 volume 参数时 `sc.files.list(subdir, {volume})` 已支持。
- `style.css`：三块补 `--badge-wrapper-bg/--badge-wrapper-fg`（暗色校验对比度，规则：白字 ≥4.5:1）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

---

**文件：**
- 创建：`web/static/video-player.js`
- 创建：`web/static/video-player.test.js`
- 修改：`web/static/index.html`（script 引入 + 播放器弹窗容器）
- 修改：`web/static/app-render.js`（视频行「▶ 播放」按钮）
- 修改：`web/static/style.css`（弹窗样式）
- 修改：`Makefile`（web-test 双清单加 video-player）

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/video-player.test.js`
预期：FAIL（`videoPlayerUrl` undefined）。

- `video-player.js`：纯函数 + 顶层挂浏览器全局（非 UMD，module.exports 供测试）：
  - `videoPlayerUrl({filename, volume, base})` → `/download?filename=<encodeURIComponent(slash→%2F)>[&volume=]`（`filename` 用 `encodeURIComponent`，斜杠转 `%2F` 保持路径结构——对齐服务端 `ValidateFilePath` 读法）。
  - `videoPlayerModalHtml({filename, volume})` → 弹窗：`<video controls autoplay src="...">` + 文件名标题 + 关闭按钮（`data-close`）。
- `app-render.js`：视频行（`is_dir=false` 且扩展名 ∈ video 集合）加 `<button data-video-play>`；委托在 `app.js` `initDynamicEventDelegation` 挂 `data-video-play` → `videoPlayerModalHtml` + 弹窗 DOM。
- `index.html`：`<script src="/ui/video-player.js">` 置于 app.js 之前（依赖序：video-player 先加载）；`#video-player-modal` 容器。
- `style.css`：`.video-modal` 样式 + 三块 token（用现有 `--modal-*` 变量，不新增色）。
- `Makefile` web-test 清单加 `video-player.js`（node --check）+ `video-player.test.js`（node --test）。

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/video-player.test.js && node --check web/static/video-player.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

---

**文件：**
- 创建：`web/static/vol-manage-format.js`
- 创建：`web/static/vol-manage-format.test.js`
- 修改：`web/static/index.html`（stats-modal 增「卷管理」tab + script）
- 修改：`web/static/app.js`（switchStatsTab 挂卷管理 tab、加载/提交/删除）
- 修改：`web/static/app-render.js`（`volumesTableHtml` 加「管理」入口）
- 修改：`Makefile`（web-test 双清单加 vol-manage-format）

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/vol-manage-format.test.js`
预期：FAIL（undefined）。

- `vol-manage-format.js`：
  - `volManageFormHtml({type, category, fields})` → 按 `fields` 渲染（`volume-select` → 底层卷下拉（值来自 `volManageFormHtml` 第二参数 `volumes`，`allow_wrapper` 决定是否含 wrapper 卷）；`enum` → `<select>`；`text`/`bool`/`number` → 对应 input）；含 `data-type` 隐藏字段。
  - `volManageListHtml({volumes})` → 现有用户卷表 + type/category 列 + 「删除」按钮（`data-delete-volume`）。
- `app.js`：stats-modal 加「卷管理」tab → `showVolManage`：`GET /api/backends`（类型 schema）+ `GET /api/volumes/user`（我的卷）→ 渲染表单+列表；创建提交 `POST /api/volumes/user`（extra 由表单字段组装）；删除 `DELETE /api/volumes/user?name=`（409 → toast 提示「有任务引用」）。
- `index.html`：stats-modal tab 增「卷管理」；script 引入 vol-manage-format.js。
- `Makefile`：web-test 双清单加 `vol-manage-format.js`/`vol-manage-format.test.js`。
- `style.css`：复用现有表单/弹窗样式，不新增 token。

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/vol-manage-format.test.js && node --check web/static/vol-manage-format.js && node --check web/static/app.js`
预期：PASS。

---

**文件：**
- 创建：`web/static/audit-rows-format.js`
- 创建：`web/static/audit-rows-format.test.js`
- 修改：`web/static/app-render.js`（任务行点开「审计」入口 + 卷内加密信息展示）
- 修改：`web/static/app.js`（任务详情弹窗接 audit 渲染）
- 修改：`Makefile`（web-test 双清单）

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/audit-rows-format.test.js`
预期：FAIL（undefined）。

- `audit-rows-format.js`：`auditRowsHtml({rows})` → 表格：type/level/step/dur_ms/bytes/bw + 加密行并排明文 vs 密文（`bytes` vs `meta.cipher_bytes`）+ 算法。
- `app-render.js` `_cloudTaskActions`（`:565-591`）加「审计」按钮（`data-audit`，有 audit 行才显示）；`app.js` 委托挂 → 弹窗 `auditRowsHtml`。
- 卷内加密信息：任务 7 的 `FileInfo` 若带 `volume_category=wrapper`，详情行展示「加密卷」徽标（复用任务 9 badge）。
- `index.html`/`Makefile`：新 JS 双清单。

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/audit-rows-format.test.js && node --check web/static/audit-rows-format.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

---

**文件：**
- 创建：`web/e2e/audit_webui_e2e_test.go`（或扩展既有 `web/e2e/volumes_e2e_test.go`）
- 创建：`test/e2e_cli_audit_test.go`（或并入既有 CLI 套件）

运行：`cd D:\workdir\leon\cocomhub\sproxy && make test-e2e`
预期：PASS（含既有全部 e2e）。

---

**文件：**
- 修改：`config.example.yaml`（`audit` 配置段 + wrapper 卷建卷示例注释）
- 修改：`docs/config.md`（`audit.*` 字段文档）
- 修改：`docs/roadmap.md`（V7 待办勾除：外部卷播放器/文件浏览已交付）

- `config.example.yaml` 加 `audit:` 段（`enabled`, `dir`, `archive: {enabled: false, volume: "", path: ""}`）——**默认不归档**；开启需选卷+路径（规格 §4.5）。
- `docs/config.md` 同步字段。
- `docs/roadmap.md` V7 勾除已交付项。

运行：
预期：EXIT=0（fmt-all → lint-all → check-ci 全绿；lint 0 issues）。

---

---

## 来源（**已移除**；如需实现细节请查 git 历史 `plans/2026-10-06-cloud-download-integrity.md`）：（任务拆解已执行完毕；仅保留设计性约束与审查重点）

## 全局约束

- 无外部依赖的校验器放 `pkg/integrity` 根包；唯一外部依赖（ffprobe）走 `pkg/integrity/ext/video/`（独立 go.mod）
- tar 校验器用标准库 `archive/tar` + `compressx`（不加依赖）
- 图片校验移植 cocom `pkg/imaging` 语义（`image.DecodeConfig`+`Decode`+Bounds 非空），不引入 cocom 依赖
- 校验点嵌入 `runRetryLoop`（runDownloadAttempt 成功分支），不单独循环
- 默认 `IntegrityMustPass=false` 放行 + 标记 `IntegrityStatus="damaged"`；`true` 阻断（failTask）
- 两次本地 checksum 一致仍异常 = 原始文件问题（不重下，按任务处置）
- 所有测试只绑 `127.0.0.1`；提交身份 `suixibing <suixibing@gmail.com>`，不加署名行
## 审查重点（Review Focus）

1. 未知类型文件（无校验器）→ 仅字节级校验，不误报 damaged（`Lookup` 返回 nil 视为通过）
2. 空文件/0 字节 → image/tar 校验器应判失败（Bounds 0 / tar 无条目），但字节级校验通过
3. 校验失败但重试耗尽 → 不无限重下（两次 checksum 一致即停，防死循环）
4. `IntegrityMustPass=true` 但校验器不存在（未知类型）→ 视为通过（无语义校验可执行），不误阻断
5. PikPak GCID 复算未命中（大文件分块自适应失败）→ 回落 ② 态（语义校验兜底），不误报权威

---

**文件：**
- 创建：`pkg/integrity/registry.go`
- 创建：`pkg/integrity/errors.go`
- 测试：`pkg/integrity/registry_test.go`

运行：`go test ./pkg/integrity/...`
预期：FAIL（registry 未定义）

扩展名匹配规则：注册 `image/*` 匹配 `.png/.jpg/.jpeg/.gif/.webp/.bmp`；`archive/tar` 匹配 `.tar/.tar.gz/.tgz/.tar.zst/.tar.br`；未知扩展 → nil。

运行：`go test ./pkg/integrity/...`
预期：PASS

---

**文件：**
- 创建：`pkg/integrity/image.go`
- 创建：`pkg/integrity/tar.go`
- 测试：`pkg/integrity/image_test.go`、`pkg/integrity/tar_test.go`

运行：`go test ./pkg/integrity/...`
预期：FAIL（Checker 未实现）

运行：`go test ./pkg/integrity/...`
预期：PASS

---

**文件：**
- 修改：`pkg/downloader/downloader.go`（Result 加 Integrity/AuthorityHash 字段 + IntegrityMode 常量 + IntegrityProvider 接口）
- 修改：`pkg/downloader/http_downloader.go`（HTTP 下载器声明 IntegrityMode）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（PikPak 声明 ModeLocalOnly + AuthorityHash）
- 测试：`pkg/downloader/downloader_test.go`、`pkg/volume/ext/pikpak/downloader_test.go`

运行：`go test ./pkg/downloader/...`
预期：FAIL（IntegrityMode 未定义）

运行：`go test ./pkg/downloader/... && go test ./pkg/volume/ext/pikpak/...`
预期：PASS

---

**文件：**
- 修改：`pkg/cloud/manager.go`（CloudTask 加 IntegrityStatus/IntegrityMustPass + TaskParams 加 IntegrityMustPass）
- 修改：`pkg/cloud/manager_task.go`（runRetryLoop 成功分支加完整性判定；CreateTask 参数透传；状态写入）
- 测试：`pkg/cloud/integrity_test.go`

运行：`go test ./pkg/cloud/ -run TestDownloadIntegrity`
预期：FAIL

TaskParams 加 `IntegrityMustPass bool`；CreateTask 透传。

- 重下判定：校验失败且非永久 → `downloadErr = errIntegrityFail`（重试，复用退避）；`shouldRetryDownload` 加 `errIntegrityFail` 认可
- 两次一致 → 永久：`checkDownloadIntegrity` 用 task 本地 checksum 累计（跨 attempt 保留）

运行：`go test ./pkg/cloud/... -run TestDownloadIntegrity && go test ./pkg/cloud/...`
预期：PASS

---

**文件：**
- 修改：`cmd/sclient/cloud_download.go`（--integrity-must-pass flag + 透传）
- 修改：`pkg/client/cloud.go`（CloudDownloadOption WithCloudDownloadIntegrityMustPass + body 字段）
- 修改：`pkg/server/cloud_download_handler.go`（请求解析 IntegrityMustPass + TaskParams）
- 测试：`pkg/client/chain_cloud_download_test.go`、`cmd/sclient/cloud_download_test.go`

运行：`go test ./pkg/client/ -run TestCloudDownloadIntegrityMustPass`
预期：FAIL

- `pkg/client/cloud.go`：`WithCloudDownloadIntegrityMustPass(v bool)` → cfg.forceIntegrity；batch/single body 加 `integrity_must_pass`
- `cmd/sclient/cloud_download.go`：`--integrity-must-pass` flag（默认 false）+ `WithChainIntegrityMustPass` + submit 透传（对齐既有三参链式）
- `pkg/server/cloud_download_handler.go`：请求 struct 加 `IntegrityMustPass bool` → TaskParams.IntegrityMustPass

运行：`go test ./pkg/client/... ./cmd/sclient/... ./pkg/server/...`
预期：PASS

---

**文件：**
- 创建：`pkg/integrity/gcid.go`（GCID 复算：sha1(concat(sha1(分块))) + 候选分块）
- 创建：`pkg/integrity/gcid_test.go`
- 创建：`pkg/integrity/ext/video/go.mod` + `video.go`（ffprobe 校验器，独立 module）
- 修改：`cmd/sproxy/`（装配 ext/video 空白导入）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（Result.AuthorityHash = FileMeta.Hash + Integrity=ModeAuthority 当 GCID 复算命中）

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：FAIL（RecomputeGCID 未定义）

候选：256KB / 512KB / 1MB / 2MB / 4MB。

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：PASS

`downloader.go`：下载完成后读文件 → `RecomputeGCID(file, candidates)` 命中官方 `FileMeta.Hash` → `result.Integrity=ModeAuthority + AuthorityHash=hash`；未命中 → `ModeLocalOnly`。

运行：`go test ./pkg/integrity/... ./pkg/volume/ext/pikpak/... ./cmd/sproxy/...`
预期：PASS

---

**文件：**
- 修改：`web/static/app-render.js`（云任务行展示 integrity 状态）
- 修改：`web/static/transfer-render.test.js`（node 单测）
- 修改：`web/e2e/ui_e2e_test.go`（Playwright e2e）

运行：`cd web/static && node --test transfer-render.test.js`
预期：FAIL（无标记）

`buildTransferRowHtml` 行徽章区（badge 旁）：`IntegrityStatus=="damaged"` → `完整性异常` 标记（黄色，`escHtml` 转义）；`"verified"` → 无额外标记（正常）。

运行：`cd web/static && node --test transfer-render.test.js`
预期：PASS

运行：`cd web/e2e && go test -run TestCloudDownloadIntegrityBadge`
预期：PASS

---

运行：`go test ./pkg/... ./cmd/... ./internal/archcheck/... && golangci-lint run ./pkg/... ./cmd/... && cd web/static && node --test && cd web/e2e && go build ./...`
预期：全绿 + lint 0

运行：`go test ./pkg/cloud/... ./pkg/integrity/... -count=1`
预期：PASS

---

## 来源（**已移除**；如需实现细节请查 git 历史 `plans/2026-10-28-unified-volume-model.md`）：（任务拆解已执行完毕；仅保留设计性约束与审查重点）

## 全局约束

- 主仓只读：所有改动在独立 worktree（`sproxy-volunify`）进行，分支基于 origin/master，禁止直接改 `/d/workdir/leon/cocomhub/sproxy`。
- 中文回复；文件 UTF-8 无 BOM；SPDX 头（`Copyright 2026 The Cocomhub Authors. All rights reserved.` / `SPDX-License-Identifier: Apache-2.0`）。
- TDD：先写红测试再实现；测试纯标准库；绑定 `127.0.0.1`。
- 测试注入一律结构体构造注入（禁全局可变 seam）——沿用 AGENTS.md 7b。
- 不把 `storage.Root` 的 32 个方法塞进 `StorageVolume`（写内核保持 Root）。默认卷 `globalRoot`/`tenantFor` 语义不动。
- `volume://`（卷内定位序列化，已有）与 `local://`（协议寻址，U4 新增）并存，文档分工。
- 本地卷 trusted 装饰启用（meta sidecar + 校验）；老文件无 sidecar → 读侧跳过校验（fail-open，不破坏）。
- secrets 视图不入通用表（R26 白名单维持）；通用 `VolumeView` 拒绝 `secrets` use。
- 测试默认 `t.Parallel()`（除明确串行需求，见 AGENTS.md R18）。
## 审查重点（Review Focus）

按「最可能伤到使用者」排序——规格是愿景文档，下面每行是规格没提、但必须钉住的行为：

1. **`VolumeView(name, "user")` 的键空间**：本地卷落 `<owner>/user/<rel>`（ResolveLocation），外部卷同。若误拼成 `user/<rel>`（无 owner 前缀）→ 多租户文件交错。→ U2 加断言测试：`VolumeView` 后 `ResolveLocation(...).FSPath()` 前缀含 `NormalizeOwner(owner)`。
2. **转存到本地卷后 `/download` 必须命中**：产物落点与读路径键空间必须一致（否则「转存成功但下载 404」）。→ U3 e2e 断言：`target_volume=本地卷` 创建任务完成后 `GET /download?filename=<rel>` 200 且内容一致。
3. **`local://` URL 的路径穿越**：`ResolveURL("local://卷/../x")` 必须被 `ResolveUserLocation` 拒（fail-closed）。→ U4 加 URL 解析测试。
4. **本地卷装配接线后容量记账不丢**：`assembleLocalVolume` 改走 `localBackend` 后，用户写本地卷仍需走卷级记账（不可因「本地卷无 CapacityFS」漏记账 → 无限额）。→ U1 断言装配后 `Set.StorageVolume(name)` 实现 `capacity.CapacityFS` 语义（或等价配额权威在 FS 层）。
5. **U2 迁移后 `externalFSFor`/`externalVolumeTransferFS` 行为不变**：转存/读路径的装饰链（Guard→Wrap→capacity→staging）不能因换实现丢失。→ U2 对比测试：改前改后同一转存产物 meta sidecar 均存在、读回校验一致。

---

**文件：**
- 修改：`pkg/volume/registry/set.go`（新增 `storageVolumes` map + `StorageVolume(name)` 访问器 + `NewSet` 收参）
- 修改：`pkg/volume/registry/set_test.go`（新测试）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 空串归一已有，无需动——确认）
- 修改：`pkg/server/volumes.go`（`assembleLocalVolume` 改走 `NewLocalBackend`，`volumeAccum` 加 `storageVolumes`）
- 修改：`pkg/server/volume_local_backend.go`（`localBackend` 补容量记账能力——见步骤 4）
- 测试：`pkg/volume/registry/storage_volume_unify_test.go`（新）

在 `pkg/volume/registry/storage_volume_unify_test.go`：

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume_LocalReturnsBackend`
预期：FAIL，`RegisterStorageVolume` 未定义

在 `pkg/volume/registry/set.go`：

`NewSet` 内部：`storageVolumes` map 初始化；为每个 `external` 条目登记（`storageVolumeOf` 或 `capacity.Backend` 已实现 `StorageVolume`）；`AddExternalVolume`/`AttachExternal` 同步登记；`RemoveExternalVolume` 同步清理。

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume`
预期：PASS

修改 `pkg/server/volumes.go`：
- `assembleLocalVolume`：把裸 `OpenLocalVolumeRoot` 改为 `NewLocalBackend(vc, rootDir, log)`（`NewLocalBackend` 已复用 `OpenLocalVolumeRoot`，含 at-rest 加密——零逻辑重复）。
- `volumeAccum` 加 `storageVolumes map[string]registry.StorageVolume`；`assembleLocalVolume` 把 `localBackend` 登记进去。
- `NewSet(...)` 调用处传新参数或在 `NewSet` 后调 `RegisterStorageVolume`。
- `roots` map **保留**（默认卷 tenant/globalRoot 仍走它——`localBackend.Root()` 暴露的 root 复用，`acc.roots[vc.Name] = lb.Root()`）。
- 确认 `localBackend` 已实现 `volume.Footprinter`/`PhysBound`（已有）→ 本地卷容量记账走 FS 层时能力不缺。

在 `pkg/server/volumes_unify_test.go`（新）：

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/ -run 'TestSet_StorageVolume|TestAssembleVolumes_LocalBackend'`
预期：PASS

---

**文件：**
- 创建：`pkg/volume/registry/views.go`（`use` 常量 + `VolumeView` 方法）
- 修改：`pkg/server/volumes.go`（`externalVolumeTransferFS`/`externalFSFor` → 委托 `VolumeView`）
- 修改：`pkg/server/files_service.go`（`ExternalVolume`/`externalSinkFor` 消费 `VolumeView`）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/volume/registry/set.go`（`VolumeView` 需要 trusted/staging 装饰——见步骤 3 的注入设计）

在 `pkg/volume/registry/views.go`：

本地卷：经 `StorageVolume(name).FS()`（`localBackend.FS()` = `NewRootFS(root)`）→ 按 use 包装饰。
外部卷：`StorageVolume(name).FS()`（`be.FS()`）→ 同 use 包装饰 → `trusted.Guard(Wrap(fs))`（与现 `externalVolumeTransferFS` 同一 recipe）。
`ViewMeta`：`trusted.Guard(fs)`（不经 Wrap/capacity）——**meta 桶是本地卷概念**：外部卷经 `StorageVolume(name).FS()` 的 Guard 视图返回（无 meta 桶语义，消费方按需跳过 meta 相关调用）；**不返回 (nil,false)**（目录浏览/控制面仍可读）。

关键：trusted Wrap 需要 `trusted.Options`（ChunkSize/Logger，来自 Handlers 配置），staging 是 per-owner（`ApplyStagingQuota(fs, trackerFor(owner))`）→ **不能在 registry 层硬编码**（registry 不 import server，也拿不到 Handlers）。

设计（装配期注入）：`Set` 新增可配置字段（构造注入，同 `NewSet` 参数）：

`NewSet` 加参 `decorator VolumeViewDecorator`（nil = 裸返回，测试友好）。装配层传：

**staging owner 问题**：`VolumeView(name, use)` 无 owner——staging 是 per-owner。两种：
(a) `VolumeView(name, owner, use)` 三参（owner 空 = 不经 staging）——U3 转存目标需要 owner；
(b) `VolumeView(name, use)` 返回**未套 staging** 的视图，staging 由消费方（已有 `h.stagingQuotaFS`）再套。
→ **选 (b)**：容量/可信/封印在装配期套好（根治 R27），staging 是 per-owner 瞬态，保持消费方 `h.stagingQuotaFS`（既有模式）。文档%2二架构图已明示 staging(owner) 在取值时。

在 `pkg/volume/registry/storage_volume_unify_test.go`：

`pkg/server/volumes.go`：
- `externalVolumeTransferFS(volName)` → `h.volSet.VolumeView(volName, registry.ViewUser)`（套 staging 由调用方或内部补）。
- `externalFSFor(volName)` 同。
- 迁移后原函数保留作为薄包装（或删，若零调用点）。**验证**：`files_phys.go`/`du.go`/`files_service.go` 的既有调用不变。

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/`
预期：全绿（含既有外部卷转存/读路径测试——行为不变）

---

**文件：**
- 修改：`pkg/server/cloud_download_handler.go`（`externalVolumeTransferFS` 调用点 → `VolumeView`；`cloudArchiveGroupToVolume` 目标解析）
- 修改：`pkg/cloud/manager_task.go`（`checkTransferVolumePreflight` 放宽：有 FS 即可，不要求 scheme 非空）
- 修改：`pkg/cloud/transfer.go`（`transferFS` 兜底：scheme 空时用 `"local"`）
- 修改：`pkg/server/routes_setup.go`（`TransferFSFor` 注入点）
- 修改：`web/static/app-render.js`（`isTransferTargetVolume` 放行 `local`）
- 修改：`web/static/app-render.test.js`（断言更新）
- 修改：`pkg/server/cloud_download_handler_test.go`（新测试）
- 新增：`test/e2e_volume_transfer_test.go`（e2e）

在 `pkg/server/cloud_download_handler_test.go`：

预期：FAIL（preflight 400「target volume not assembled」——当前 `volSet.External` 对本地卷 nil）

`pkg/cloud/manager_task.go`：`transferFS(volume)` 不再只认 scheme 非空——FS 非 nil 即可；scheme 空时按 `local` 处理（URL 生成 `local://<卷>/<rel>`，无需注册——见 U4）。

`pkg/server/cloud_download_handler.go`：`cloudArchiveGroupToVolume`/`archiveGroupToVolume` 的目标 FS 解析改走 `h.volSet.VolumeView(t.TargetVolume, registry.ViewUser)`。本地卷返回 Guard(Wrap(NewRootFS(root)))（trusted 对本地启用——用户裁定 4）。`target_volume` ACL 校验（`volumeAllowedFor`）不变。

`web/static/app-render.js` `isTransferTargetVolume`：`t !== 'local'` 移除（保留 `t !== 'secrets'`）。
`web/static/app-render.test.js`：`isTransferTargetVolume({type:'local'})` 断言改为 `true`。

运行：`node --test web/static/app-render.test.js`
预期：PASS

新增 `test/e2e_volume_transfer_test.go`：启动真实二进制，`POST /api/cloud/download`（target_volume=默认卷），轮询任务完成，`GET /download?filename=<rel>` 内容一致。

运行：`go test -count=1 ./pkg/server/ ./pkg/cloud/` + `make web-test`
预期：全绿；`make test-e2e`（或该 e2e 包）通过

---

**文件：**
- 修改：`pkg/server/volume_local_backend.go`（`RegisterLocalBackend` 加 `"local"` protocol）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 分派已有；`ResolveURL` 对 local 卷处理）
- 修改：`pkg/volume/registry/set.go`（`ResolveURL` 增加 local:// 分支）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/server/cloud_download_handler.go`（转存 URL 生成用 `local://`）

在 `pkg/volume/registry/storage_volume_unify_test.go`：

预期：FAIL（`无后端声明协议 "local"`）

`pkg/server/volume_local_backend.go`：`registry.RegisterBackend(volume.TypeLocal, factory, "local")`。
（注：`RegisterBackend` 已允许 `TypeLocal` 注册——见 `backend.go` doc「local 允许注册」；protocol "local" 不与既有冲突——`schemeBackends` 无 "local"。）

`registry.Set.ResolveURL`：scheme=="local" → 按 authority（卷名）经 `StorageVolume(name).FS()` 读；路径经 `volume.ResolveUserLocation`（权限门 + 路径安全）换算 FS 键（`<owner>/user/<rel>` 或匿名）。与既有 `URLResolver` 断言分支并列（local 卷无 `URLResolver` 接口——本地卷不经 `OpenURL`，直接 FS）。

 `pkg/cloud/transfer.go`：`transferFS` 返回 scheme 空时归一为 `"local"`（与 U3 步骤 3 一致）；`targetURL("local", vol, rel)` 生成 URL。

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/cloud/ ./pkg/server/`
预期：全绿（含既有 URL 解析测试——行为不变，新增 local:// 分支）

---

**文件：**
- 修改：`internal/archcheck/set_root_calls.tsv`（预算缩减）
- 修改：`internal/archcheck/set_root_accessor_gate_test.go`（若口径需更新）
- 修改：`pkg/server/handlers.go`（`volSet.Root` 存在性探测 → `HasLocalRoot`/`VolumeView`）
- 修改：`pkg/server/nested_enclosure.go`（`Root` 探测 → `HasLocalRoot`）
- 修改：`pkg/server/volume_retention.go`（`Root` → `HasLocalRoot`）

`grep -rn "volSet.Root(\|set.Root(" pkg/server/ pkg/volume/ pkg/files/ cmd/ --include="*.go" | grep -v _test`
→ 标识「存在性探测」（`if rt := h.volSet.Root(v.Name); rt != nil`）类 → 改 `HasLocalRoot`；「真需 root 方法」（`Root.Abs/Stat/SealAt/ListOwners/OpenFileEncrypted`）→ 保留。

`pkg/server/handlers.go:688`、`pkg/server/nested_enclosure.go:383`、`pkg/server/volumes.go:847/986`、`pkg/server/volume_retention.go:106`：
`volSet.Root(x) != nil` → `volSet.HasLocalRoot(x)`。

`internal/archcheck/set_root_calls.tsv`：扣除迁移行（或减 max_calls），同步注释。

运行：`make archcheck` + `go test -count=1 ./pkg/server/ ./pkg/volume/ ./pkg/files/`
预期：archcheck 全绿（预算收缩后不红）；功能测试全绿

---

**文件：**
- 修改：`docs/architecture.md`（统一卷模型章节）
- 修改：`docs/api.md`（转存 target_volume 支持本地卷）
- 修改：`docs/cli.md`（若 CLI 有转存相关说明）
- 修改：`docs/config.md`（若配置有变化）
- 新增：`docs/archive/unified-volume-model.md`（精华归档）

新增章节：`StorageVolume` 唯一身份 + `Set.VolumeView(name, use)` 唯一入口 + 按用途视图表 + local:// scheme 分工。

`target_volume` 描述：从「外部卷」改为「任意存储卷（含本地卷）」；URL 形态 `local://<卷>/<rel>`。

`cloud-download`/`archive` 相关 CLI flag 说明；若配置无变化则跳过。

运行：`make check-ci`（R9/R15 门禁）
预期：全绿

按功能维度沉淀：统一模型的价值（新后端只实现 5 方法）、按用途封装的坑（secrets 不入通用表 / staging per-owner 在取值时 / Root 32 方法不塞接口）、迁移经验。

---
