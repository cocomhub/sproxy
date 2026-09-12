# 文件服务子包抽取 设计

> 本规格是「把 `pkg/server` 的文件操作抽取为领域子包」的权威设计。实施计划见 `docs/superpowers/plans/` 下同名计划。

**目标**：把文件服务（list/stat/download/upload/delete/rename/mkdir/rmdir + 分块传输 + 校验和 + 版本）从 `pkg/server` 抽成**领域包 `pkg/files`（含子包）**，并把其中**可被其他领域复用**的抽象提升为顶级包；全程保持**功能与测试场景逐字不变**。

**技术栈**：Go 1.26，纯标准库（`log/slog`、`go/parser`/`go/build`、`os/exec` 用于门禁），不新增依赖。

**背景依据**：`pkg/server` 现有 142 个文件 / 21,490 行非测试代码，其中文件操作占 8 个文件约 4,400 行。表面积过大导致：① 新表面（Y 一期的 `remote_read`，未来的 `remote_write`）倾向于**各自重写**上传/下载，而非复用；② 领域边界不可见。Y 一期 DoD-6 排查（隧道 `handleStream` 缓冲整个响应体）暴露出该隐患的严重性。

---

## 1. 目标与非目标

### 目标
1. 文件服务成为**独立领域包** `pkg/files`，内部按能力分子包。
2. 其中**跨领域复用**的抽象提升为**顶级包**（路径安全、校验和、容量核算）。
3. 为 Y-C 提供**挂载点**：新 HTTP 表面挂载文件服务，而非重写。
4. 拆分阶段**零逻辑变更**，功能与测试场景可机械核对。

### 非目标（本次不做）
- 不拆 `pkg/server` 的其余职责：cloud / auth / config / share / volumes API / hub / stats / sync / archive / credentials。
- **不改任何 HTTP 契约**（状态码、错误消息、响应头、分块语义、配置默认值）。
- 不在拆分阶段做任何逻辑优化或行为趋同。
- 不修改类型名（`ChecksumStore` / `UploadStore` 等），只改包名与导入路径。

---

## 2. 设计原则（判据，供后续 PR 照此判断）

| 编号 | 原则 | 含义 |
|---|---|---|
| **P1** | 强关联 → 进领域包子包 | 包是**给读者的表面积**。只与某领域强相关的实现细节，放进该领域包的子包；门外人无需知道其存在（对齐 `pkg/tunnel/{mux,xfer,hub,p2p}` 的既有做法） |
| **P2** | 可被其他领域复用 → 顶级抽象包 | 若一个抽象会被本领域之外的模块使用，它属于顶级包，而不是某个领域的内部细节 |
| **P3** | 领域名词命名 | 包名取**功能领域**（`pathguard` 路径安全、`checksum` 校验和、`capacity` 容量），不取实现形态（`xxstore`、`xxutil`） |
| **P4** | 逐字不变与重设计分离 | 拆分阶段零逻辑变更；**一切重大逻辑变更统一放到重设计阶段** |
| **P5** | 包名现在改，类型名留后 | 包名现在按 P3 定；类型名中的 `xxStore` 等到重设计阶段统一为领域名（避免污染拆分阶段的可核对性） |

**不要为了多级而多级**：只有**强关联**（同一领域的内部实现）才进子包；一次性把平铺包改成嵌套而不满足 P1 的，属于违规。

---

## 3. 目标包布局

```
pkg/pathguard/              顶级｜路径安全策略             ← pkg/server/validate.go
pkg/checksum/               顶级｜文件校验和台账           ← pkg/server/checksum_store.go
pkg/storage/                顶级｜已存在：存储根/租户/布局
pkg/storage/capacity/          └ 子包｜容量与占用核算       ← pkg/server/storage_manager.go
pkg/quota/                  顶级｜已存在：配额池与 Scope
pkg/volume/                 顶级｜已存在：卷 ACL 纯域
pkg/volume/registry/           └ 子包｜运行时卷集合装配与定位 ← pkg/server/volumes.go（装配部分）
pkg/files/                  文件服务域根｜Deps 接缝 + 路由 + 读写处理器
pkg/files/chunked/             └ 子包｜分块会话与块传输      ← upload_store.go + chunked_upload.go + chunked_download.go
pkg/files/version/             └ 子包｜文件版本存储          ← version.go（存储部分）
pkg/client/                 客户端（阶段 C 对称抽取）
pkg/server/                 装配层（导入方向单向）
```

### 3.1 分类依据（实测的使用者统计）

| 抽象 | 现文件 | 实测使用者 | 判定 |
|---|---|---|---|
| 路径安全 `ValidateFilePath` | `validate.go`（100 行，2 个函数） | **15 个文件**：archive、checksum、chunked_download、chunked_upload、delete、dirs、download、list、remote_read、rename、share、upload、version、volumes_api | **P2 顶级** |
| 校验和 `ChecksumStore` | `checksum_store.go`（194 行） | 文件面 + **`cloud_download`** + `storage_manager` + `version` | **P2 顶级** |
| 容量核算 `StorageManager` | `storage_manager.go`（449 行） | chunked、**cloud ×3**、**sync**、**stats**、config_api、quota_reconcile、upload_store | **P2** 但强关联存储 → `pkg/storage/capacity`（P1 子包） |
| 分块会话 `UploadStore` | `upload_store.go`（1095 行） | **仅** `chunked_upload` + 装配 | **P1 子包** |
| 文件版本 | `version.go`（842 行） | **仅** `upload_handler` + 自身 | **P1 子包** |
| 运行时卷集合 `volumeSet` | `volumes.go`（675 行） | 文件面 + cloud + share + volumes_api | **P1 子包**（强关联卷领域） |

### 3.2 已存在的接口（对接缝有利，随包迁移）

- `checksum_store.go` 已有 `ChecksumStoreIface`
- `upload_store.go` 已有 `UploadStoreIface`

二者原样迁入各自新包，**不改名、不改方法集**。

### 3.3 依赖方向（目标层级）

```
L0  pkg/pathguard
L0  pkg/checksum
L1  pkg/storage   pkg/quota   pkg/volume            （均已存在）
L2  pkg/storage/capacity        ← pkg/checksum, pkg/storage, pkg/quota
L2  pkg/volume/registry         ← pkg/storage, pkg/quota, pkg/volume
L3  pkg/files/chunked           ← pkg/storage, pkg/quota（capacity 经窄接口注入）
L3  pkg/files/version           ← pkg/checksum, pkg/storage
L4  pkg/files                   ← L0–L1 顶层包全部 + L2 子包的**能力窄接口**
L5  pkg/server   pkg/client     ← 装配层（唯一可直接导入任意子包者）
```

> **`←` 表示装配注入关系，不等于 import 关系。** 子包（`pkg/*/xxx`）受门禁规则②（R2）保护：**任何非父域子树、非装配层的包（含兄弟领域包 `pkg/files`）不得直接 import**。领域包需要子包能力时，在**消费方包内**声明只含所需方法的**窄接口**，由装配层注入结构满足的实现（零适配代码）。
>
> **本表在任务 5 起飞前据实测订正**：原写 `L4 pkg/files ← L0–L3 全部`，与规则②**自相矛盾**——`pkg/volume/registry`/`pkg/storage/capacity` 是子包，`pkg/files` 不在其允许导入者内。实测确认 `pkg/files` 的导入集为 `pkg/{pathguard,checksum,quota,storage,volume}` + stdlib，**不含任何子包**。
>
> R2 的长期选项（为子包加"具名例外表"，或按判据 P2 把 `registry`/`capacity` 提升为**顶级包**）留给**阶段 D**——后者会让已落地的任务 3/4 与本节再次变动，代价需与收益一并评估。

**已知方向证据**：`UploadStore.SetStorageMgr(*StorageManager)` ⇒ `pkg/files/chunked` 需要 `pkg/storage/capacity` 的能力，故 chunked 在 capacity 之上（**抽取后经窄接口注入，不是 import**）；`storage_manager.go` 接收 `ChecksumStoreIface` ⇒ capacity 依赖 checksum（这里 `pkg/checksum` 是**顶级包**，故仍是 import）。

> **注意区分**：同层级的 import 依赖（顶级包之间、以及领域包对其下层顶级包）由规则①约束；**子包一律不 import，走窄接口注入**，规则②管的是"谁能 import 子包"。层级表以 §8 的门禁为唯一事实源，**每片 PR 落地时把该片抽出的包登记入表**（尚未抽出的包不登记）。

**R2 只约束生产代码**：测试文件（如 `pkg/files/dirs_test.go`，`package files`）为构造真实 `VolSet` 而导入 `pkg/volume/registry` **不违规**——`internal/archcheck` 解析的是 `go list` 的**非测试导入集**。

---

## 4. 依赖接缝（Deps 的收缩）

抽取前，文件操作 handler 对 `*Handlers` 的真实外部依赖实测为约 12 项；抽出下层包后，**大部分退化为对下层包的直接 import**，`Deps` 收缩为「装配注入类」：

| 原依赖 | 抽取后形态 |
|---|---|
| `h.checksumStoreFor` | → 直接持有 `pkg/checksum` 的实例获取器 |
| `h.uploadStoreFor` | → `pkg/files/chunked` 内部持有 |
| `h.storageMgr` | → 经**窄接口**注入（`pkg/storage/capacity` 是**子包**，R2 不允许 `pkg/files` 直接 import；装配层注入 `*capacity.Manager` 满足） |
| `h.volSet` + `tenantFor`/`tenantOf`/`volumeTenant`/`defaultVolumeAllows`/`locateOwnerFile`/`locateForRead`/`resolveDownloadPath` | → **`Deps.VolSet` 收窄为消费方接口**（`pkg/volume/registry` 是**子包**，同上；装配层注入 `*registry.Set` 满足，零适配代码）。**任务 5 实测订正**：本行原写"直接持有 `*registry.Set`"，与规则②冲突 |
| `h.quotaScopeFor` | → 直接持有 `pkg/quota` 的 Scope 获取器 |
| `h.logger` | → `Deps.Logger` |
| `h.metrics` | → `Deps.Metrics` |
| `h.cfgPtr` | → **窄函数**（如 `MaxUploadBytes() int64`），**不得**出现 `*Config` —— 那会让 `pkg/files` 反向依赖 `pkg/server`，**被门禁规则③判红**。（**任务 5 实测订正**：本行原写 `Deps.Config func() *Config`，与规则③直接冲突） |
| `h.RecordAudit` | → `Deps.Audit` |
| `h.uploadingFiles` | → `Deps.Uploading *sync.Map` |

**`pkg/server` 侧为一行薄适配**（`h.listFiles` → `h.files.List` 等），路由注册表**逐字不变**。按项目规程「抽象先薄包装委托保障一致 → 最终直接用新抽象」，本阶段采用薄适配并在重设计阶段评估是否去除。

---

## 5. 阶段与 PR 切分

每片都是**功能完整、可独立验证**的交付物；每片结束都必须通过 §6 的四条机械核对。

### 阶段 A｜准备：先抽 `fileops` 依赖的包（逐字不变）

| PR | 内容 | 交付物 |
|---|---|---|
| **A-1** | `pkg/pathguard`（`ValidateFilePath` + `hasServiceInternalPrefix`）；15 处调用点改导入 | 路径安全成为顶级包，`pkg/server` 不再自己实现 |
| **A-2** | `pkg/checksum`（`ChecksumStore` + `ChecksumStoreIface`）；含 `cloud_download` 在内的调用点改导入 | 校验和台账成为顶级包 |
| **A-3** | `pkg/storage/capacity`（`StorageManager` + `StorageCategory` + `ReconcileFunc`）+ `pkg/volume/registry`（运行时卷集合装配与定位） | 存储域、卷域各收一个子包 |

### 阶段 B｜抽 `pkg/files` 领域（逐字不变）

| PR | 内容 |
|---|---|
| **B-1** | `pkg/files/chunked`（`upload_store.go` + `chunked_upload.go` + `chunked_download.go`） |
| **B-2** | `pkg/files/version`（版本存储部分） |
| **B-3** | `pkg/files` 根：**只读面**（`listFiles` / `stat` / `download` / 分块下载的路由接线）+ `Deps` 接缝 + 路由注册导出 |
| **B-4** | `pkg/files` 根：**写面**（`upload` / `rename` / `delete` / `mkdir` / `rmdir`） |

### 阶段 C｜客户端对称（逐字不变）

| PR | 内容 |
|---|---|
| **C-1** | `pkg/client` 的文件操作与分块能力抽取为同构子包，与服务端共用 DTO 雏形 |

### 阶段 D｜适度重设计（**重大逻辑变更统一在此**）

| PR | 内容 |
|---|---|
| **D-1** | 统一分块语义与 DTO、消除双端重复、边界行为趋同、类型名领域化（`checksum.Ledger`、`chunked.Sessions`）、评估去除薄适配 |
| **D-2** | 文件服务暴露**域操作 API**：HTTP 处理器降为薄适配；新表面（remote_read/write）挂**域 API** 而非改写请求 |

### 之后
- Y-C（`feature/y-read-transport`）**rebase** 到含 A–C 的 master，T6 起挂载 `pkg/files`。

---

## 6. 「功能与测试场景一致」的机械保证

拆分阶段的正确性不靠"跑测试看看"，而靠四条**可执行**核对。每片 PR 必须全部通过。

1. **测试随包迁移、不复制**：`*_test.go` 跟随被测代码迁移并改包名；`pkg/server` 内**不得保留**旧副本。
2. **用例名集合守恒**：迁移前执行 `go test -list '.*' ./pkg/server/` 记录用例名集合，迁移后对两侧集合做**末段归一化**（去掉包名前缀）后比对，必须守恒。这是"测试场景一致"最直接的证据。
3. **e2e 零改动**：`test/**` 一行不改（走真实二进制），是本阶段行为不变的最强证据。
4. **路由表逐条比对**：重构前后把路由注册导出为可枚举清单（`[]struct{Method, Pattern, Handler}`），逐条 diff，必须完全一致。

---

## 7. 「逐字不变」的定义

**允许**：
- 文件搬迁；包名与导入路径变更。
- 可见性最小调整（因跨包而必须导出/收回的标识符）。
- `pkg/server` 侧新增薄适配转发（一行）。

**不允许**：
- 控制流、错误语义、HTTP 状态码、响应头、错误消息文案、分块语义（chunk 大小/偏移/校验）、配置默认值发生任何变化。
- 顺带的"小修小补"（哪怕明显是缺陷）——记入重设计阶段或独立议题。

---

## 8. 新增门禁：分层与子包可见性校验

新增 `internal/archcheck`（放 `internal/` 而非 `pkg/`，避免成为公共 API），以测试形式运行，**违规即红**。

**实现**：`os/exec` 调 `go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./...` 解析导入图（纯标准库；与仓库既有 e2e「构建真二进制」的 exec 用法同源）。

**规则三条**：
1. **分层方向**：层级表中 L(n) 的包不得导入 L(>n) 的包（表见 §3.3）。
2. **子包可见性**：子包（如 `pkg/files/chunked`、`pkg/storage/capacity`）只允许**父域子树**与**装配层**（`pkg/server`、`pkg/client`、`cmd/`）导入；其余包一律不得直接导入。装配层是必要例外——路由注册在 `pkg/server`，它必须引用子包的处理器。
3. **新包依赖须登记**：本工作新增的包（`Managed`）不得导入 `pkg/` 下**未登记**的包；反之，使"新包在依赖图中的位置"必须显式声明。

**为什么不做「无环」断言**：Go 编译器已保证——成环必定编译失败，显式断言纯属冗余。三条规则保留的理由是它们覆盖了编译器**管不到**的两类违规，两者都能**编译通过**：
- **方向**：`pkg/pathguard`(L0) 导入 `pkg/storage`(L1) 不成环、可编译；
- **封装**：`pkg/checksum` 导入 `pkg/files/chunked` 同样可编译，却破坏了子包边界。

注意：规则 3 的作用域**只限本工作新增的包**。若让存量包也承担"依赖必须登记"的义务，登记一个存量包就会拖出它整条子图（`pkg/tunnel` → `xfer`/`mux`/`hub`…），门禁无法落地；而限定作用域后，规则 3 仍精确拦下最需要防的方向——新包反向导入 `pkg/server`（它不在层级表中）。

**维护责任**：新增包必须登记层级表，否则门禁报"未登记包"。

---

## 9. 与 Y 一期的衔接

- Y-C 的 T4/T5 已完成并推送（`feature/y-read-transport`）。本议题的 A–C 阶段合并入 master 后，Y-C rebase 继续 T6。
- T6（`pkg/remote` 客户端）与 B-3/B-4 的只读面**直接复用** `pkg/files`，不另写下载路径——这正是本议题要消除的"各自实现"。
- **过渡期挂载形态**：A–C 阶段文件服务以 **HTTP Handler 形状**暴露（保持逐字不变）；重设计阶段（D-2）降为**域 API**，此时 remote_read 从"改写请求 + 伪造 actor"改为"注入受限 Deps 直接调用域 API"。

---

## 10. 风险与取舍

| 风险 | 评估与处置 |
|---|---|
| 与在途分支冲突 | **低**。Y-C 自 T3 起把「既有读路径文件零改动」当硬约束（`list_handler.go`/`download_handler.go`/`handlers.go` 每轮机械核实零 diff），故 rebase 面小 |
| "逐字不变"难自证 | 由 §6 四条机械核对覆盖；其中用例名守恒做了末段归一化以抵消包名变化 |
| 门禁维护成本 | `internal/archcheck` 需在新增包时登记；收益是"缩小表面积"从文档约定变为**可执行**约束 |
| 搬迁量 | A+B 合计约 4,600 行（含测试更多）；分 7 片 PR，每片独立可验证、可回退 |
| 已发现的既有瑕疵 | `NewStorageManager(dir, maxBytes, _ ChecksumStoreIface, logger)` 第三参数被忽略（`_`）。**不在拆分阶段修**（属于逻辑变更），记入重设计阶段 |

---

## 11. 自检记录

- **占位符扫描**：无"待定/TODO"。§3.3 层级表声明"每片 PR 登记该片抽出的包"，是**增量登记机制**的明确定义，非占位。
- **内部一致性**：§3.1 分类依据与 §3 布局一致；§5 的 PR 与 §3 的包一一对应；§6 与 §7 的"逐字不变"定义互相支撑。
- **范围检查**：单片计划不可覆盖（A–D 共 9 片），故本规格**只覆盖阶段 A–C 的抽取**（对应一个计划），阶段 D 为重设计阶段、**另立计划**——本规格为 D 提供目标形态（§9）。
- **歧义检查**：已明确选择并写死——① `capacity`/`registry` 归 `pkg/storage`、`pkg/volume` 之下（判为强关联子包）；② 分层与子包可见性门禁**做**；③ 拆分阶段只改包名不改类型名。
