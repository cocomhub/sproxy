# 远程访问面（读+写）与 sync/remote 收敛 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 按已确认的 A/B/C 三项决策，把"远程文件访问"收敛为**单一抽象 `sync.FS`**，并为**远程写**（Y 二期）铺平道路——先固抽象（P1）、再域化文件服务（P2）、最后开写批次（P3）。

**架构：** `sync.FS`（7 方法，读写）是唯一抽象；`pkg/remote` 是它的 **mesh 版实现**，`HTTPTransport`/`LocalFS` 是另两种；`pkg/sync` 退回纯逻辑；B 侧远程面**只做授权 + 调域 API**。

**技术栈：** Go 1.26；纯标准库；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-13-remote-access-architecture-design.md`

---

## 全局约束

- **行为逐字不变（P0–P2）**：HTTP 契约（状态码/响应头/错误文案/分块语义）、配置默认值、授权判定，一律不得变化。P3 是**新增能力**（写），不改既有读语义。
- **不允许顺带的"小修小补"**——发现缺陷写进报告，由控制者落进规格。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues**（前者根 module、后者 10 个子 module）。
- 提交时**只 `git add` 本任务改动的文件**；message 用**多重 `-m`**；**不加署名行**。
- **不要改动计划/规格文件**：实施中的经验与口径修正写进报告，由控制者落进计划。
- 每次 Bash 调用在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`。
- **工作分支：每片一个分支，从最新 master 切出**，该片 PR 合并后再开下一片。
- **`go build ./...` 只覆盖根 module**；**必须**另跑 **`make build-all`**。
- **archcheck 登记**：新顶层包必须**同时**写入 `Levels` 与 `Managed`（`Managed∖Levels` 已有断言把守）；子包另需 `ParentDomain`。
- **python 编辑陷阱（本仓已两次踩坑）**：文本模式写入会把 LF 转 CRLF ⇒ 核对 ②（用例名守恒）**误报用例丢失**。改完 Go 文件一律 `gofmt -w`；改文档用**二进制模式**读写。

## 四条机械核对（每片必跑）

```bash
git diff --stat db99de06 -- test/          # ① 应为空（本系列沿用既有披露）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)   # ② 必须无 "-" 行
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)   # ③ 必须为空
go test ./internal/archcheck/               # ④（含 R1–R7）
```

## 验证命令（每片必跑）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ internal/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
make test-e2e
```

---

## P0｜Y-C 一期：rebase 与合并（**不缩水的既有计划**）

**依据：** 规格 §6 P0 行。T4 的短写/截断修复是**写批次的安全前置**，必须先落 master。

- [ ] `feature/y-read-transport` rebase 到最新 master。实测**冲突面只有 2 个文件**：`cmd/sproxy/root.go`（master 侧 3+/2−）、`pkg/server/config.go`（master 侧 1+/1−）。
- [ ] **重锚点**：Y-C 计划「现有代码锚点」引用的行号已全部失效（`pkg/server/list_handler.go:190`、`download_handler.go:255`、`handlers.go:222 tenantFor`、`validate.go:31` …）——按新位置改写（`pkg/files/*`、`pkg/server/remote_read.go`），**只改文档不改代码**。
- [ ] 跑 `make check-ci` 确认 T4/T5 仍绿；开 PR 合并（T4 传输原语 + T5 loopback listener）。
- [ ] T6 `pkg/remote`：**按 `sync.FS` 形态一次性设计接口**（只实现 3 个读方法；写 4 方法返回 `ErrUnsupported`）——即把 P1 的抽象决定前移到 T6，避免二次改接口。

**DoD：** 四条机械核对全过；`make check-ci` 绿；T4/T5/T6 的测试在 `-race` 下通过。

---

## P1｜抽象收敛（**零行为变更**）

**依据：** 规格 §3.2、§5.1/5.2/5.5/5.6。

- [x] **P1-a｜确立并记录抽象**（随 #210 交付）：在 `pkg/sync` 的包文档中明确「`FS` 是唯一的远程文件操作抽象；本包**不含**网络实现」，并把 `http_transport.go` 的定位写清（HTTP 版 `FS` 实现）。
- [x] **P1-b｜网络实现迁出 `pkg/sync`**（#210：`pkg/sync/httptransport` + `pkg/sync/internal/fsutil`；pkg/sync 降为 G0）：`HTTPTransport` 移到 `pkg/sync/httptransport`（子包，判据 P6①「可复用的扩展工具集合」）或 `pkg/syncfs`（按实施时实测耦合面择一，**必须写明选择理由**）。`LocalFS` 留在 `pkg/sync`（非网络）。
      - 沿用 `checksum.Reader` 先例：可先加转发/别名再删旧，保证每步可编译可回退。
      - 1009 行测试随之搬迁（用例名守恒）。
- [x] **P1-c｜`syncmgr` 载体模型**（#210：`RemoteKind` + 分组字段 + `KindOrDirect`）：`RemoteKindDirect`/`RemoteKindMesh`；现有 `RemoteConfig{Name,URL,AK/SK}` **保留为兼容构造**（`direct` 的唯一实现），配置与持久化格式不变。
- [x] **P1-d｜`syncexec` 按 Kind 选 FS**（#210：mesh 明确 `ErrMeshTransportNotWired`，不回落 direct）：`direct` → HTTP 版；`mesh` → `pkg/remote`（P0 的 T6 产物）。
- [ ] **P1-e｜`remote://` 句柄统一**（**推迟到 P3**：需扩展 `/api/sync` 入参语义，属契约扩展而非零行为变更）：`pkg/remote.ParseRef` 作为唯一解析器；`syncmgr.Job.Src/Dst` 允许 `remote://` 前缀（解析失败 fail-closed）。

**DoD：** ① 四条机械核对全过（③ 路由表逐条一致是硬约束）；② `make lint`/`lint-all` 0 issues；③ `go build ./...`/`make build-all` 过；④ `go test ./pkg/... ./internal/...` 全绿；⑤ e2e 绿；⑥ **零行为变更**：既有 sync 用例（HTTP 直连路径）全绿且未改断言。

---

## P2｜D-2：`pkg/files` 域操作 API

**依据：** 规格 §1.5、§5.3、§5.9。**这是本系列最大工程**，按读面→写面→目录/批量切片。

**域 API 目标形状**（`pkg/files`，出参为领域类型而非 HTTP）：

```go
// 读
func (s *Service) List(owner, volName, dir string) ([]FileInfo, error)
func (s *Service) Stat(owner, volName, remotePath string) (FileInfo, error)
func (s *Service) Open(owner, volName, remotePath string) (io.ReadCloser, FileInfo, error)
// 写（*IfUnchanged 保留 checksum 门禁不变量）
func (s *Service) WriteFile(owner, volName, remotePath string, r io.Reader, size, mtime int64, expectedCS string) (WriteResult, error)
func (s *Service) RenameIfUnchanged(owner, volName, from, to, expectedCS string) error
func (s *Service) DeleteIfUnchanged(owner, volName, remotePath, expectedCS string) error
func (s *Service) Mkdir(owner, volName, dir string) error
func (s *Service) Rmdir(owner, volName, dir string) error
```

- [x] **P2-a｜读面域化**（已交付 PR #211：List/Search/StatPath/OpenPath + 9 条 HTTP 契约钉住测试，重构前后双跑均绿）：`List`/`Stat`/`Open` 落地；`ListFiles`/`SearchFiles`/`Stat`/`Download` 处理器降为「解析请求 → 调域方法 → 写响应」的薄适配。**响应字节必须逐字不变**（含 `X-File-Checksum`/`X-File-MTime`/`X-Volume` 头与 Range 语义）。
- [ ] **P2-b｜写面域化**：`WriteFile`/`RenameIfUnchanged`/`DeleteIfUnchanged`/`Mkdir`/`Rmdir` 落地；`Upload`/`Rename`/`Delete`/`Mkdir`/`Rmdir` 处理器降薄。**checksum 门禁、mtime、原子改名、版本保存、配额、文件锁、卷路由**全部留在域方法内。
- [x] **P2-c｜批量族**（已交付 PR #222）：`BatchDelete`/`BatchRename` 改为**在域方法之上循环**——
  `processBatchRenameItem` 调 `RenameFile`、`processBatchDeleteItem` 调 `DeleteFile`，两族各自只剩
  「调用 → 文案映射 → 结果聚合」（原两处共 ~200 行复制逻辑删除）。要点：
  - **文案映射不比对中文**：`HTTPError` 新增可选 `Reason`（机器可读原因码，P3 远程写面同样要用），
    批量族按 `Reason` 分派回自己的历史文案（rename 两项：缺 checksum / 建父目录失败；delete 三项：
    无效路径 / 缺 checksum / 删除失败）。
  - **两族历史差异改为显式入参**：`DeleteFileInput.AllowMissing`（缺文件按幂等成功）、
    `SkipFileLock`（不因并发上传把整批变 409，**遗留差异，已标注 TODO**）、`RenameFileInput.Origin`
    （审计 Detail 来源标记）。
  - **审计归一化**：`renameAuditDetail(base, to, origin)` 统一为 `<base>[（batch）]: to=<to>`
    （原先单条 checksum 拒绝行不带目标、批量行带 (batch) 但单条不带 ⇒ 两族信息量不一致）。
  - **顺带补齐**：批量删除原先**不记 `RecordDelete` 计量**、缺文件/源缺失时**不写审计** ⇒ 现与单条族一致。
  - **两处输入校验归一化**（批量族向单条族看齐，已在契约测试中钉住）：空 `from`/`to` 走单条族文案；
    批量删除改为「先校验 checksum 入参再触盘」。
  - **命名偏差（如实记录）**：本条计划原写 `RenameIfUnchanged`/`DeleteIfUnchanged`，实际落地为
    `RenameFile`/`DeleteFile`——checksum 前置条件以**入参**表达（`ExpectedChecksum`）而非写进方法名；
    已合并 API 不再改名（避免无谓 churn），P3 的 `sync.FS` 写方法名（`Rename`/`Remove`）亦不依赖该命名。
  - TDD：先写 4 条红灯契约测试（批量删除计量、批量幂等缺失审计、批量源缺失审计、单条 checksum
    拒绝审计带目标）→ 实现 → 4 条转绿；另 2 条钉住输入校验归一化。
- [ ] **P2-d｜分块族与域 API 的关系**：明确 `/upload/init|chunk|complete` 的会话层**复用** `WriteFile` 的共同内核（临时文件 + 收尾原子 rename），不复制写语义。
- [x] **P2-e｜B 侧只读面切到域 API**（已交付：`delegate` 直调域方法，删除请求改写与伪造 actor；装配层新增 `downloadPathForRemote` 显式解析器；Y-C 规格 AD-8 已标注为历史形态）：`pkg/server/remote_read.go` 的 `delegate` 从「改写请求 + 伪造 actor」改为直调域方法；**授权三步不变**；审计不变。
- [ ] **P2-f｜文档**：`pkg/files` 包文档补「域操作 API 与 HTTP 面是两层：HTTP 处理器是薄适配」；废弃/删除只服务旧形态的注释与遗留。

**DoD：** ① 四条机械核对全过；② `make lint`/`lint-all` 0 issues；③ `go build ./...`/`make build-all` 过；④ `go test ./pkg/... ./internal/...` 全绿（**用例名零丢失**，允许新增）；⑤ e2e 绿；⑥ `pkg/files` 覆盖率不回退、零覆盖函数保持 0；⑦ `remote_read` 不再出现 `withActor(` 伪造路径（源码级检查）。

---

## P3｜写批次（Y 二期）

**依据：** 规格 §5.3/5.4/5.7/5.8、§7。

- [ ] **P3-a｜授权轴**：`pkg/volume` 的 `MeshReader` 加 `Scope`（`read|write|rw`，缺省 `read`）；`AuthorizeMeshRead` 保持原名与语义（**读**），新增 `AuthorizeMeshWrite`；配置解析 + `Validate` + 文档同步。**旧配置语义零变化**。
- [ ] **P3-b｜B 侧写 listener**：**独立服务名**（如 `volwrite`）与**独立路由白名单**（只注册 4 个写 op）；读服务物理上仍只注册 `GET`/`HEAD`（AD-7 非黑名单法）。装配于 `cmd/sproxy`。
- [ ] **P3-c｜A 侧 `pkg/remote` 实现 4 个写方法**：`WriteFile`（spool + SHA-256 + 流式提交）、`Rename`/`Delete`（**先 `Stat` 取 checksum**）、`MakeDir`。
- [ ] **P3-d｜`syncexec` 支持 `remote://` 目标**：push/pull 可把对端指定为 `(node, vol)`，走 mesh 版 `FS`；`direct` 保留。
- [ ] **P3-e｜审计与文档**：`mesh_write` 事件；配置示例；Y-C §11 的「谁持有写权」在此**明确规定**为「单属主 + B 侧文件锁」（无分布式协调）。

**DoD：** 同 P2 的 ①–⑥，且额外：⑦ 读服务路由表**逐条不含写方法**（源码/路由清单双证）；⑧ 写路径**必然经过** `pkg/files` 域方法（源码级检查 + 反向探针：临时改域方法应使远程写用例变红）；⑨ `-race` 下 mesh 写用例通过。

---

## P4｜收敛

- [ ] `direct` 降级为 `RemoteTarget.Kind` 的普通取值；文档说明「HTTP 直连为兼容路径，不再新增能力」。
- [ ] 旧 `sync_remotes` 配置继续可用（回归用例）。

---

## 收尾（全部阶段后）

- [ ] 更新 `2026-09-11-y-cluster-read-design.md` §11（写批次已实现，指向本规格与本计划）。
- [ ] 更新 `2026-09-12-file-service-extraction-design.md` 阶段 D（D-2 已交付）。
- [ ] 全量回归：`make check-ci`、`make test-e2e`、覆盖率门禁。
