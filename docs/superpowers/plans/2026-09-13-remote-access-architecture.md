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
- [x] **P2-b｜写面域化**（已交付：#214 `WriteFile`、#217 `MakeDir`/`RemoveDir`、#220 `RenameFile`/`DeleteFile`；
  `Upload`/`Rename`/`Delete`/`Mkdir`/`Rmdir` 处理器均已降为薄适配）：**checksum 门禁、mtime、原子改名、
  版本保存、配额、文件锁、卷路由**全部留在域方法内。命名落地为 `RenameFile`/`DeleteFile`（计划原文的
  `RenameIfUnchanged`/`DeleteIfUnchanged` 是 checksum 前置条件，实际以入参 `ExpectedChecksum` 表达；
  见 P2-c 的「命名偏差」说明）。契约证据：`read_contract_test.go` / `write_contract_test.go` /
  `rename_delete_contract_test.go` 均**重构前后双跑皆绿**。
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
- [x] **P2-d｜分块族与域 API 的关系**（已交付 PR #223）：分块 complete 的「落盘后副作用」收敛到与
  单次上传同一份内核——`recordCompleteMetadata` 改为调 `recordUploadSuccess`（mtime + checksum 台账），
  并删除 `UploadComplete` 里**重复的** `cs.Set`（同一 (rel, checksum) 写两遍）。
  收尾原子 `rename` 两路径本就共用 `atomicRenameRoot`。**门禁**：新增
  `internal/archcheck/upload_side_effect_test.go`（设置 mtime 的调用在 `pkg/files` 非测试源码里
  **恰好一处**且必须在 `write_ops.go` 的 `recordUploadSuccess` 内；先红后绿）。**行为契约**：新增
  `pkg/files/chunked_complete_contract_test.go` 钉住分块 complete 的 mtime 落盘与台账写入
  （此前全仓无任何 `ModTime()` 断言）。
  **有意保留的三处差异**（已逐条写进 `chunked_upload.go` 注释，防「顺手统一」）：① 版本保存时机
  （分块＝目标存在即备份；单次＝仅 checksum 不同时备份）；② 配额结算形式（分块＝Commit(total) +
  ReleaseUsage(prev) 显式对账；单次＝`UploadRoute.Commit` 的 Adjust 差分；终态相同）；
  ③ 卷池结算两处一致。
- [x] **P2-e｜B 侧只读面切到域 API**（已交付：`delegate` 直调域方法，删除请求改写与伪造 actor；装配层新增 `downloadPathForRemote` 显式解析器；Y-C 规格 AD-8 已标注为历史形态）：`pkg/server/remote_read.go` 的 `delegate` 从「改写请求 + 伪造 actor」改为直调域方法；**授权三步不变**；审计不变。
- [x] **P2-f｜文档**（已交付 PR #223）：`pkg/files` 包文档（`service.go` 顶注）新增「两层：域操作 API
  与 HTTP 面」章节，含**域方法 → HTTP 面**落地清单与「为什么必须两层」；顺带清理仅服务旧形态的
  注释（两处指向已删除 `resolveListDir` 的引用）。

**DoD：** ① 四条机械核对全过；② `make lint`/`lint-all` 0 issues；③ `go build ./...`/`make build-all` 过；④ `go test ./pkg/... ./internal/...` 全绿（**用例名零丢失**，允许新增）；⑤ e2e 绿；⑥ `pkg/files` 覆盖率不回退、零覆盖函数保持 0；⑦ `remote_read` 不再出现 `withActor(` 伪造路径（源码级检查）。

---

## P3｜写批次（Y 二期）

**依据：** 规格 §5.3/5.4/5.7/5.8、§7。

- [x] **P3-a｜授权轴**（已交付 PR #224）：`pkg/volume` 的 `MeshReader` 加 `Scope`（`read|write|rw`，缺省
  `read`）；`AuthorizeMeshRead` 保留原名与语义（**读**），新增 `AuthorizeMeshWrite`；两者共用
  **单一判定入口** `authorizeMesh`（三重约束：node/fingerprint/owner 命中 + scope 命中 + owner 过卷 ACL）。
  配置侧 `VolumeMeshReaderConfig.Scope` + `Validate` 响亮拒绝未知值 + `SetDefaults` 归一为规范小写形 +
  `parseVolumeACL` 透传（未知值丢弃条目并留痕，与畸形指纹同策略）。**取值集合单一事实源**：
  `volume.NormalizeMeshScope`（配置校验与授权判定共用，杜绝「配置放行但授权拒绝」）。
  **语义决策**：读不隐含写、写不隐含读；未知 scope fail-closed（读写都拒，绝不「不认识就当 read」）；
  空值/纯空白 ≡ 未配 ≡ read（零回归，两层同一规则）。
  **证据**：`pkg/volume/volume_mesh_scope_test.go`（22 个子用例矩阵）+ 配置层 4 条测试；
  **变异验证**已确认测试会咬人（「写隐含读」与「未知值回落 read」两种变异各被对应用例捕获）。
  **旧配置语义零变化**：既有 `volume_mesh_test.go` 的 read 矩阵全绿。
- [x] **P3-b｜B 侧写 listener**（已交付 PR #225 + 本 PR）：**独立路由白名单**（只注册 4 条 POST：
  `/remote/write|rename|delete|mkdir`）与**独立开关/监听**（`remote_write` 段，强制 loopback）；
  读服务物理上仍只注册 `GET`/`HEAD`（AD-7 非黑名单法）。写面**只做授权 + 调域 API**
  （`files.WriteFile`/`RenameFile`/`DeleteFile`/`MakeDir`），不复制任何写语义。
  要点：
  - **owner 恒来自配置**（`MeshReaderFor` 反查），绝不接受请求参数指定；
  - 授权走 `AuthorizeMeshWrite`（三重约束，scope 必须授予写）；
  - **pin 列表只收「授写」指纹**（`write|rw`）⇒ 只读对端连写面握手都建立不了（写通路物理隔离），
    与只读面 pin 策略各管一边、互不放大权限；
  - 启动校验 fail-fast：非 loopback / 空 listen / 超时非正 / **没有任何授写条目** ⇒ 拒绝启动；
  - body 上限复用本地上传同一硬上限 `internal/size.UploadBodyLimit`（远程面不得绕过上限）；
  - 装配于 `cmd/sproxy`（与只读 listener 同构的关闭路径：ctx 感知 accept）。
  TDD：handler 端 6 条契约/端到端测试 + 3 种变异验证；listener 端 3 条中测（真握手、真 pin）
  + 2 种变异验证（pin 收全量 / 删校验），变异均被对应用例捕获。
  未做（如实记录）：A 侧服务发现用的**独立服务名** `volwrite` 常量属 P3-c（`pkg/remote` 侧），
  本片只落实 B 侧的独立路由与独立监听。
- [x] **P3-c｜A 侧 `pkg/remote` 实现 4 个写方法**（已交付：见 `pkg/remote/write.go`）：
  `WriteFile`（**spool + SHA-256 + 单次流式提交**：对端要求 checksum 前置，故先落临时文件并同时
  算摘要；`size` 与实读不符即报错＝调用方 bug 早暴露）、`Rename`/`Delete`（**先经读面 `Stat` 取
  checksum**，对端写面无 stat；源不存在/无 checksum 则不发写请求）、`MakeDir`。
  要点：独立服务名常量 `ServiceNameWrite = "volwrite"`；`WithWriteDialer` 注入写面拨号器，
  **写面与只读面链路缓存分面隔离**（不同 listener/白名单/pin 策略，绝不复用同一链路）；
  未配置写面时 `ErrWriteNotConfigured` **立即** fail-closed（不发任何请求，含读面探测）。
  `remoteFS` 的 4 个写方法仅做「路径归一 → 调 Client」；`sync.FS` 接口自 P1 固化，实现填充
  **未改任何调用方**（防返工收益兑现）。死代码清理：原「未实现」哨兵 `ErrUnsupported` 已删除
  （唯一返回者就是那 4 个写桩）；既有测试 `TestRemoteFS_ReadAndUnsupportedWrites` 更名并改断言
  （如实披露：`→ TestRemoteFS_ReadsAndWritesWithoutWriteDialer`）。
- [x] **P3-d｜`syncexec` 支持 `remote://` 目标**（接缝已交付；装配接线与 P1-e 见下）：
  `Executor` 新增 `MeshFSFactory` 接缝（`MeshFSFactory(ctx, RemoteConfig) (sync.FS, closeFn, error)`）
  + `SetMeshFSFactory`（沿用既有 `SetXxxResolver` 风格）：`kind=mesh` 由**装配层注入**的工厂构造
  mesh 版 `sync.FS`——`pkg/syncexec` 不依赖 `pkg/tunnel/mesh` 子 module、也不自己拨号；
  未注入时保持 `ErrMeshTransportNotWired`（**绝不回落 direct**）；工厂错误原样上抛（不吞、不降级）。
  `newRemoteFS` 增 `ctx` 入参（工厂要拨号）。
  TDD：3 条新用例（工厂被调用且收到完整配置 node/volume/pins/transport + FS 真被写入 + 任务结束
  调 close；工厂错误可 `errors.Is` 判定且不报「未装配」；未注入仍 fail-closed）；**2 种变异**验证
  （不调 close、吞掉工厂错误）均被捕获。
  **未做（如实记录）**：① 装配层把工厂接到 `cmd/sproxy`（需 `pkg/remote` + 中继/直连拨号器与
  hub 服务发现接线，属装配片）；② **P1-e**（扩展 `/api/sync` 入参语义，使任务可直接声明
  `kind/node/volume/peer_pins/transport`）——两者合并为后续一片交付。
  装配片的前置已就绪（本 PR）：`remote.RelayDialer` 的依赖收窄为**最小接口** `RelayClient`
  （`MeshServices` + `RelayStream`），装配层可注入自有实现、测试可用替身。
  **接线设计（已探明，留给下一片执行）**：A 侧中继经**本机 hub API**（`/api/hub/services` +
  `/api/relay/stream`，`*client.FileClient` 即实现之），故 `cmd/sproxy` 需
  ① 导出服务端**自用凭据**访问器（现 `pkg/server` 只有未导出的 `bestFirstCredential(ring)`，
  被 xfer listener 使用；其 `Ring.Snapshot()` 含 SK 明文，故自用签名可行）；
  ② 由 `cfg.Addr`+`cfg.TLS` 派生本机 base URL（自签证书需 `client.WithInsecureTLS`）；
  ③ 用 `remote.New(readDialer, WithIdentity(xfer 身份), WithPeerPin(node, pins...), WithWriteDialer(writeDialer))`
  构造并返回 `c.FS(ref)`；`transport=webrtc` 需 `pkg/tunnel/mesh` 子 module（cmd/sproxy 不导入）
  ⇒ 该值在此装配中明确报错，留待 CLI 侧注入。
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
