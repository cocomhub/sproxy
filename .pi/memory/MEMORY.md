<!-- 2026-09-19 19:45:00 [01a0b2aa] -->
# sproxy 项目记忆（从全局拆分，2026-09-19）

> 仅含 sproxy 专用内容（版本线/门禁/release-please/benchmark/mux/quota/chunked/PR 记录）。
> 通用策略（授权/合并纪律/提交身份/Web UI 测试等）在全局 `~/.pi/agent/memory/MEMORY.md`。
> 在 `D:/workdir/leon/cocomhub/sproxy` 打开 pi 时由 josephkern/pi-memory 注入「全局 + 项目」两层。

<!-- 2026-09-19 20:30:00 [01a0b2aa] -->
## 提交/发布纪律 skill（2026-09-19 新增）

- **项目 skill**：`.pi/skills/sproxy-release-discipline/SKILL.md`（pi 项目级 skills：`.pi/skills/`，受信任后注入）——
  提交信息格式（`type(scope)!:` 而非 `feat!(scope)`，后者 release-please 解析抛错）、破坏性变更标注与
  BEGIN_COMMIT_OVERRIDE 补救、squash 信息源、版本号影响。凡 commit/合并/release 相关任务按需加载。
- AGENTS.md「执行偏好」段已加引用行。

<!-- 2026-09-14 10:23:11 [01a09c79] -->
## sproxy 版本线与 tag 实况（订正，2026-09-14）

> 早前一条记忆称「远端零 tag、未推送」——**实测相反，已作废**。以此条为准：

- 远端**已有** annotated tag `v0.1.0` … `v0.11.0`（共 11 个，`git ls-remote --tags` 可证）；v0.11.0 → 47ce4b0e。
- 这些 tag 在 2026-09-13 21:26–21:31 被推送，**触发了 8 个 Release run（v0.4.0–v0.11.0）全部 failure**——根因是 `.goreleaser.yaml` 用了废止字段（已在 PR #249 修复）。
- **历史 tag 不回溯补 GitHub Release**（重跑旧 run 会检出旧 tag 的旧配置，无意义）；`gh release list` 目前为空。
- `CHANGELOG.md` 的 AI 回溯稿（0.1.0–0.11.0 + compare 链接 + `[Unreleased] ### Removed`）已随 PR #249 合入 `master`。
- **嵌套模块 tag 不存在**：`cmd/sproxy/vX.Y.Z`、`cmd/sclient/vX.Y.Z` 尚未创建（Go 官方要求嵌套 module tag 必须为 `cmd/sproxy/vX.Y.Z` 形式才能 `go get`）——属 PR-2 待办。
- 注意：`cmd/*/go.mod` 带相对 `replace ../../` + `require ... v0.0.0` ⇒ 即使打了嵌套 tag，`go install .../cmd/sproxy@vX` 仍不可用（代理会忽略 replace）；本轮只保证 tag 与 CHANGELOG 自洽。

<!-- 2026-09-14 13:04:34 [01a09c79] -->
## Go accept 循环的隐式陷阱（2026-09-14 实证，sproxy）

- **Go 的 net 层只内部重试 `EINTR`/`EAGAIN`/`ECONNABORTED`**（见 `internal/poll/fd_unix.go` 的 `FD.Accept`），
  **`EMFILE`/`ENFILE`/`ENOBUFS`/`ENOMEM` 会原样上抛**。自定义 accept 循环若把这些当致命错误直接 `return`
  且**不关闭 listener**，listener 仍绑定、内核 backlog 继续完成握手，但再无人 `Accept`
  ⇒ 表现为「端口仍可连、服务已静默死亡」（`net.Dial` 一直成功）。正确做法（与 `net/http.Server.Serve` 一致）：
  可重试错误指数退避重试，致命错误退出前先 `Close()` listener，并暴露「accept 已停止」的确定性信号供测试等待。
- **诊断这类 flake 要区分「调度饥饿」与「逻辑缺陷」**：本次用 64 个 CPU hog + `GOMAXPROCS=1` 实测
  watcher 关闭 listener 仅需 12–44ms ⇒ 排除 `-race` 慢导致的超时，确认为逻辑缺陷。
- **测试断言不要用 `net.Dial` 轮询判断「listener 已停止」**：既依赖调度，又会因端口被其它并行测试
  （`127.0.0.1:0`）重新绑定而假红；应等确定性信号 + 断言 listener 已关闭（第二次 `Close` 返回 `net.ErrClosed`）。
- 相关落地：sproxy PR #251（fix/server accept 健壮性）、PR #253（`make test-packages` 分组 `-timeout` 统一 ≥60s）。

<!-- 2026-09-14 18:23:23 [01a09c79] -->
## release-please 的 `[Unreleased]` 是陷阱（2026-09-14 源码级确认，sproxy）

- **结论：`CHANGELOG.md` 不应保留 `## [Unreleased]` 段，应直接移除。**
- 机制（`googleapis/release-please` `src/updaters/changelog.ts`）：
  `DEFAULT_VERSION_HEADER_REGEX = '\n###? v?[0-9[]'` —— 它取**第一个版本标题**作插入锚点，把新版本段插到它**前面**。
  `## [Unreleased]` 因 `[` 命中该字符类 ⇒ 成为锚点 ⇒ ① 新版本段被插到它**上面**（实测 0.11.1）；
  ② 它**从不被消费/清理**，写进去的内容**永远不会进入任何版本**；③ 逐次下移成噪音。
- 相关已知问题：release-please issue #2613（H1 `# Changelog` 与空行的位置会影响插入位置）。
- 落地：sproxy 已移除该段（PR #255），首个版本标题变为 `## [0.11.0]`；门禁 **R12** 断言 CHANGELOG 不得含 `[Unreleased]`。
- 同时新增：`changelog-sections` 支持 `remove`→Removed、`deprecate`→Deprecated，**删除对外 API 用 `remove(<scope>): ...` 提交类型**表达（免人工补条目、免被 release-please 重建 release PR 时覆盖）。发布流程见仓内 `RELEASING.md`。

<!-- 2026-09-14 21:36:12 [01a09c79] -->
## 本批后续（2026-09-14 夜，D1-c/D3 收官）

**D1-c（#265）**：`cmd/sclient/p2p_manual.go`(367 行) 手工 SDP 信令下沉 **`pkg/tunnel/p2p`**（新建包）。接缝改造两处：① `cli.IOStreams` → 本包窄接口 `p2p.UI{Out,Err,In}`（**隧道层不反向依赖 pkg/cli**；nil = 不输出/不读），CLI 侧 `p2pUI()` 做唯一一次适配；② 常量 `manualSignalingTimeout` → `p2p.ManualSignalingTimeout`。并显式声明编译期契约 `var _ webrtc.Signaler = (*ManualSignaler)(nil)` / `(*ManualStdioSignaler)(nil)`。平台特化文件迁为 `manual_signal_{unix,windows}.go`。

**D1-e 判定不做（重要结论）**：`cmd/sclient/output.go`(374 行 formatter) **不应**下沉 `pkg/cli`——formatter 需要 `pkg/client` 类型（Levels 里 **G2**），而 `pkg/cli` 是 **G0 叶子**（`internal/archcheck/layers.go` 的 Levels 表显式登记、零 pkg/* 依赖），R1 分层规则禁止 G0 导入 G2。且用户规则本身写明 cmd 负责「参数解析/装配/**输出**」⇒ formatter 属 cmd 职责，保留。

**D3 四处全部完成（全部纯搬迁、零 API 变更）**：#266 `pkg/cloud/manager.go` 2327→6 文件；#267 `pkg/client/client.go` 1896→6 文件；#268 `pkg/server/handlers.go` 1547→5 文件；#269 `pkg/server/config.go` 1492→4 文件。生产代码最大文件 2327 → ~1213（`pkg/syncmgr/manager.go`；未列入原清单的 >1000 行文件：`client/chunked.go` 1209、`webrtc/webrtc.go` 1171、`files/chunked_upload.go` 1096、`files/chunked_store.go` 1089）。

**搬迁验证方法论（可复用，务必照做）**：纯搬迁最危险的是「悄悄漏一段而测试恰好没覆盖」。每次必须做两层独立验证：① 顶层声明集合（`^func|^type|^var|^const` 行）排序后 diff 必须一致；② 非注释/非空行排序后 diff **只允许剩 import/package 行**。本次四次都过了（例：cloud 45 行差异全为 import；handlers 0 行差异；config 16 行全为 import）。工具脚本在 `build/split_go_file.py` + `build/fiximports.py`（本地、gitignore）。

**搬迁踩坑**：① 从第 1 行开始的片段会连同原文 `package`/`import` 一起复制 ⇒ 生成文件里出现两个 package（脚本已统一跳过「前导段」）；② import 裁剪**必须编译器驱动**（未使用 import 在 Go 里是编译错误，反馈最可靠），启发式 `\bname\.` 会假阳（`m.storage.` 命中 `storage.`）；③ **包名 ≠ 路径末段**：`gopkg.in/yaml.v3` 的包名是 `yaml`（末段 `yaml.v3`），漏判会静默丢 import；④ goimports 分组：`gopkg.in/...` 与 `github.com/cocomhub/...` 同组（std 后两段），不是第三组；⑤ 注意**按文件名断言**的门禁（`helper_impl_drift_test.go` 的 TestNormalizeOwner_DelegatesToStorage 要求 `normalizeOwner` 留在 `pkg/server/handlers.go`）——搬前先 grep `"<file>.go"`。

**CI flake 处置实录**：#269 的 Benchmark job 9m2s 超 8 分钟上限被判 fail ⇒ 按硬规则 `gh run rerun <run-id> --failed` 重跑，随后 15/15 全绿。另注意：**本地并发跑 `go test ./...` 时会与 `make lint` 抢编译资源**，会看到随机 lint 失败——串行重跑即 0 issues，别误判为代码问题。

<!-- 2026-09-14 20:26:38 [01a09c79] -->
## sproxy 门禁体系与口径（2026-09-14 落地，跨会话）

**新增/修改的门禁（都做过变异验证）**
- **R13 门禁自身守卫**：`internal/archcheck/gate_wiring_test.go` —— cover-check 不得用 `bc`、必须 fail-closed、必须按 COVER_THRESHOLD；deadcode-check 必须存在/过滤 `.deadcodeignore`/exit 1/被 ci.yml 调用；配方内 echo 必须 ASCII（Windows CP936 会把中文渲染成 `鍙戠幇...`）。
- **R14 测试固定等待棘轮**：`internal/archcheck/test_sleep_ratchet_test.go` —— 冻结每文件 `time.Sleep(` 计数，只减不增（2026-09-14 现状 **161 处 / 62 文件**，上限常量 `testSleepTotalBudget`）；未登记文件预算 0；门禁自身文件排除（否则自报 +11）。替代方案 `pkg/testutil.WaitFor(t, timeout, cond, msg...)`（`waitTB` 窄接口；**注意 `Fatalf` 在替身上不 Goexit，必须显式 return**，否则死循环）。
- **R15 文档漂移**：`internal/archcheck/docs_cli_flags_test.go` —— `cmd/sclient/root.go` 的每个 persistent flag 必须在 `docs/cli.md` 出现 `--name`；解析数 <10 直接失败（防正则失配假绿）。
- **R16 仓库卫生**：`internal/archcheck/repo_hygiene_test.go` —— CONTRIBUTING/SECURITY/LICENSE 存在、非占位（≥400 字符）、README 链接、SECURITY 给出私下渠道且告诫勿公开。
- **`make deadcode-check`（B1）**：按 `.deadcodeignore`（ERE，路径分隔符 `[/\\]`）过滤后失败判定，挂在 CI **Lint job**。**范围实测口径**：只覆盖 `./cmd/sproxy ./cmd/sclient` 可达图；`./...` 会产出 2000+ 行噪声（把库包导出面当根），库包单列作入口直接 `no main packages`；库包内部死代码由 R11 墓碑清单守。`make deadcode` 仍保留为信息输出。
- **`make cover-check`（B2）**：已从 `bc -l` 改为 `awk` 比较并 fail-closed（bc 缺失时旧写法在 Windows 上**恒 PASS**）。
- **R11 墓碑**：`internal/archcheck/dead_symbols_test.go` 已改为**纯 Go 目录遍历 + 词边界**（不再依赖 `git grep`/`.git`，且覆盖未跟踪新文件），自检 `TestScanDeadSymbol_ScopeAndWordBoundary`。

**本批已完成的清理**：relay/preview 改走 SDK（#257/#258）、rename TOCTOU 收口 + `AtomicRename` 不再删目标（#259）、非测试 TODO 审计收敛 13 处（#260）、--hub wss→https + 死代码 `getCloudServerURL` + `cloudfilename.ReadEntriesFromFile`（#261）、上述门禁（#262/#263）、CONTRIBUTING/SECURITY + `MeshSignalToken` 零引用导出删除 + auth_token 术语清理（#264）。

**语义/接口变更须知**：
- `storage.Root.AtomicRename` **保持替换语义**（上传覆盖依赖它），但**绝不删除目标**（旧慢路径 `Remove(dst)` 会数据丢失——Windows 实测）。
- `RenameFile` 现在对 from/to 两个 rel 非阻塞取 FileLocks，冲突返回 409「文件正在移动/上传中，请稍后重试」。
- relay 管理命令的 `--hub` 派生 HTTP 地址：`ws://`→`http://`、`wss://`→`https://`，丢弃 path/query。

**剩余路线图（未做）**：D1-c `cmd/sclient/p2p_manual.go`(367 行) 信令下沉 `pkg/tunnel/p2p`；D1-e `cmd/sclient/output.go`(374 行) formatter 下沉 `pkg/cli`；D3 拆分超大文件（`pkg/cloud/manager.go` 2327、`pkg/client/client.go` 1896、`pkg/server/handlers.go` 1543、`pkg/server/config.go` 1485）；C1 本地分支 26+/远端未合并 9/他人 stash 4（**删除需用户确认**）；dependabot #148/#152（用户明示暂缓）。

<!-- 2026-09-14 21:38:29 [01a09c79] -->
## 更正（2026-09-14 夜）：上一条「剩余路线图（未做）」中的 D1-c/D1-e/D3 已处置完毕

上一条目末尾的「**剩余路线图（未做）**」段落所列为当时的待办；其中三项现已了结，请以下述结论为准：

- **D1-c 已完成**（#265）：`cmd/sclient/p2p_manual.go` 已下沉 `pkg/tunnel/p2p`（接缝：`cli.IOStreams` → `p2p.UI`，常量 → `p2p.ManualSignalingTimeout`）。
- **D1-e 判定不做**（非遗留）：`output.go` formatter 需 `pkg/client`（G2），而 `pkg/cli` 是 Levels 表登记的 **G0 叶子**，R1 禁止 G0→G2；且 formatter 属 cmd 的「输出」职责。
- **D3 四处全部完成**（#266/#267/#268/#269）：cloud/manager.go、client/client.go、server/handlers.go、server/config.go 均已按职责拆分（纯搬迁、零 API 变更，逐行比对验证）。

**仍然未做（保持待办）**：C1 本地分支 26+/远端未合并 9/他人 stash 4（**删除需用户确认**）；dependabot #148/#152（用户明示暂缓）；未列入原 D3 清单的 >1000 行生产文件（syncmgr/manager.go 1213、client/chunked.go 1209、webrtc/webrtc.go 1171、files/chunked_upload.go 1096、files/chunked_store.go 1089）。

<!-- 2026-09-14 23:59:30 [01a09c79] -->
## 分支/stash 清理执行结果（2026-09-14 夜，C1）

**本地分支 26 → 2**（删除 24 个，全部有「已合并」证据）：
- 22 个：与 GitHub **已合并 PR 的 headRefName 同名**（docs/files-deps-options-design#198、docs/remote-access-architecture#207、feat/remote-fs-client#209、feat/server-mesh-config#236、feat/webrtc-instance-ice-config#234、feature/y-read-acl#180、fix/xfer-send-atomicity#216、refactor/{client-layout#194,credentialstore-to-accesskey#202,extract-cloud-core#204,extract-downloader#203,extract-syncmgr#201,files-deps-migrate-assembly#197,files-deps-runtime-and-options#196,files-read-domain-ops#211,files-write-coverage-and-checksum-ssot#195,files-write-domain-ops#214,remote-read-domain-api#212,slogutil-ssot#205,sync-fs-transport-layering#210,tenant-cache-wiring#200,tenant-resolve-sink#199}）。
- 2 个：`feature/y-read-transport`、`yc-rebase` —— 无同名 PR，但 6/7 个提交主题全部命中 master 的 squash 提交 **`bf5bac59 feat(remote): Y-C 一期 rebase 落地…(#208)`**（git cherry 因 squash 恒为 `+`，不可用作判据）。
- **保留** `rp252`（本地跟踪 `origin/release-please--branches--master`，含 0.11.1 审校草稿；发布后删）与 `master`。

**stash 4 → 0**：4 个均为旧特性分支 WIP，判定内容已被 master 取代——@0 设计文档（master 同文件已演化到 4A 落地后版本）、@1 otel 子模块（原 `pkg/tunnel/tracing/ext/otel` → 已落地到 **`pkg/telemetry/ext/otel`**，go.sum 100%、go.mod 94% 一致）、@2 仅 go.work.sum churn、@3 otel 装配 WIP（唯一独有文件是**改名前旧路径**版本，master 已有 `pkg/telemetry/ext/otel/provider_test.go` 覆盖）。
**删除前已导出可恢复备份**：`build/stash-backup/stash{0..3}.patch|.stat|.meta`（含 base 提交与 subject）——注意 `build/` 被 **`make clean` 删除**，需长期保留请先拷出仓库。

**仍未处置**：远端 `feature/goedel-go-optimize`（1 提交，给 Makefile 加 goedel-go optimize 目标；master 的 Makefile 无 goedel 引用 ⇒ **未合并**，等待用户决定）；dependabot #148/#152（用户明示暂缓，远端分支保留）。工作树干净、无 stash、无其他本地分支。

<!-- 2026-09-15 00:46:14 [01a09c79] -->
## 又一处「空转假门禁」：`make notest`（2026-09-14 夜修复，#271）

**两道缺陷叠加 ⇒ 该门禁自上线起从未生效**：
1. **调用方不带参数**：Makefile 的 `notest` 直接 `scripts/check-test-files.sh`，脚本是 `for pkg in "$@"` ⇒ 零次迭代 ⇒ 永远打印 `OK: all packages have test files`。
2. **忽略清单实现反了**：原用 `find "$pkg" -maxdepth 0 -not -path "./$line"` ⇒ 被忽略时输出为空 ⇒ 反被判为「未忽略」⇒ `.notestignore` 从未生效（补上参数后立刻误报 4 个 `tools/*` 包）。

**修法**：Makefile 传 `go list ./...`（`sed 's|^github.com/cocomhub/sproxy|.|'`）；脚本对**空参数 fail-closed**；忽略清单改 **glob 正向匹配**（容忍带/不带 `./` 前缀）；`build/*` 登记进 `.notestignore`（`go list` 会把 `build/tmp_db` 当包）；并把 `make notest` 接进 **CI Lint job**（此前只在本地 `make check-ci` 里且毫无作用）。

**修好立刻暴露 3 个真实缺口**（都已处置）：
- `pkg/sync/internal/fsutil`（88 行，**无测试**）→ 新增 16 例 `SanitizeRelPath` 安全边界 + 5 例 `CopyWithCtx`（含「中途取消保留已拷贝部分」）；
- `pkg/testutil/syncmock`（406 行，**无测试**）→ 新增 list/stat/download/upload/SnapshotFiles 深拷贝/mkdir-rename-delete 往返测试（它是 8+ 个 sync/server 测试的地基）；
- `build/*` 产物目录。

**踩坑（CI 抓到）**：**重写脚本会丢可执行位**（100755 → 100644）⇒ GitHub 上直接执行报 `Permission denied`（exit 126）。修法双保险：`git update-index --chmod=+x` 恢复 + Makefile 改用 `bash scripts/xxx.sh` 调用（与 `scripts/test-vault.sh` 既有约定一致，不依赖 mode）；R17 门禁同步断言「必须以 `bash scripts/check-test-files.sh` 调用」。

**新增门禁 R17**（`internal/archcheck/notest_gate_test.go`）：断言 notest 目标必须传包列表且以 `bash` 调用、脚本必须对空参数失败、忽略清单必须正向匹配、CI 必须调用、`.notestignore` 必须登记 `build/*`。变异验证：临时加无测试包 → FAIL 并点名；无参数 → FAIL 并给用法。

另核对：CI 中唯一的 `continue-on-error: true` 在 benchmark dashboard 发布步骤（`Store benchmark result`），**不在** `make bench` 本身（bench 失败仍红），属正当用法。

<!-- 2026-09-15 04:03:25 [01a09c79] -->
# synctest 气泡实证结论（2026-09-15，pkg/tunnel/hub 实测）

1. **`synctest.Wait()` 不推进虚拟时钟**——它只自旋等"气泡内其它 goroutine 持久阻塞"；
   虚拟时钟只在气泡**空闲**（所有 goroutine 都停驻）时推进。
   ⇒ 「让时钟前进 N」必须让主 goroutine 停在**虚拟 timer** 上（`time.Sleep` / `<-time.After` / ctx 截止等待）；
   = 用 Wait 轮询等 timer ⇒ 永不触发 ⇒ 无限自旋挂死（曾把 TestPersister_ScheduleDebounceRealTimer 挂到 401s）。
2. **真实 I/O 阻塞不算 durably blocked**：httptest 的 Serve goroutine、TCP accept —— synctest.Wait() 永远等不齐 → 气泡内挂死。
   ⇒ **气泡只适用于纯内存 fixture**（channel/pipe/虚拟 timer）。已验证成功：
   - `TestPersister_ScheduleDebounceRealTimer`（AfterFunc debounce + 真实文件落盘在回调内，短 I/O 可完成）
   - `TestSignalQueue_WaitTwoConcurrentWaiters_BothWake`（纯内存 waiter 注册同步）
   实测失败（已还原）：`TestHubTCP_AcceptCtxCancel`（真实 accept）、`TestHubSignaler_OfferAnswerRoundTrip`（httptest 长轮询）。
3. 气泡改造手法：**提取函数法**（`synctest.Test(t, helper)`），函数体零重缩进；import 加 `"testing/synctest"`。
   不能用脚本对函数体整段重缩进（曾把文件改坏花括号不平衡）。
4. 注释里写 `time.Sleep(xxx)` 字面量会污染棘轮计数（精确子串匹配），注释要用无字面量表述。
5. 探针（/tmp/stprobe/probe2_test.go）可用于快速复现结论：AfterFunc + Wait 轮询循环 ⇒ timer 永不触发。

<!-- 2026-09-15 05:03:25 [01a09c79] -->
# Windows 防火墙弹窗（sclient e2e，2026-09-16）

- 根因：mesh 直连 webrtc 信令监听 `resolveSignalListenAddr` 在 SignalAddr 为空时绑定**主局域网 IP**（F2：通配收敛到私网 IPv4，不绑全接口）——非回环监听即触发防火墙授权弹窗。
- **测试侧修法**：e2e helper 与进程内 NodeConfig 显式 `SignalAddr/signal-addr = 127.0.0.1:0`（flag 帮助本就写明该用途；extraArgs 可覆盖）。生产不动——LAN 直连就是要绑私网。
- 记住：本仓测试铁律「127.0.0.1 loopback」在 mesh 直连信令这条**旁路通道**上曾被漏掉；新增任何会起监听的测试 helper 时要检查所有 listener 的绑定 host。

<!-- 2026-09-15 06:40:36 [01a09c79] -->
# PR #272 squash 合并（2026-09-14/15 完成态）

- squash commit：`13c2852a`，subject `perf(test): 消除固定等待并以条件等待/并行化治理 flake（136→38 处，e2e -race 墙钟 174s→58s，单元套件降至 42s）`——perf 类型 → release-please 会进 CHANGELOG 的 Changed 段（满足 R12 约束）。
- 分支 `test/flake-sleep-batch1` 远端随 merge --delete-branch 删除，本地已清理；reles-please 已触发（origin/release-please--branches--master forced update，落 v0.11.x）。
- 台账与门禁收尾状态：棘轮 38（零未登记）、lint 0 issues、`docs/testing/virtual-time-conversions.md` 完整。
- 遗留待办（下一片）：F3「e2e bind:0→close→复用」端口 TOCTOU 端口重复（改用 root_test 端口绑定型）；F4 mux retransmitLoop 泄漏债（AGENTS.md 已知债，气泡化后失效模式变硬）；release PR #252 仍是 OPEN 待审校后才发布 v0.11.1；远端 `feature/goedel-go-optimize` 待用户决定删除。

<!-- 2026-09-15 20:16:17 [01a0a4d0] -->
# Benchmark job flake 根因与三片修复（2026-09-15，session 01a0a4d0）

**根因（实测取证，非猜）**：`pkg/client` benchmark 的 mock `/upload` 把每个 payload `os.Create` 到 `b.TempDir()`（runner 系统盘），每趟 CI 写 ~2.5 GiB ⇒ runner 脏页回写被节流后写带宽塌到 ~0.15 MB/s ⇒ 1 MiB op 从 5 ms 变 6.49 s、4 MiB 变 29.38 s（耗时与字节数成正比是判据）⇒ 单 benchmark 需 20+ 分钟，6 分钟 `timeout-minutes` 必然 cancel。只读的 `BenchmarkDownload` 同窗口仍 3.8 ms/op（排除 CPU/网络）。近 60 次 CI 有 9 次 ≥360 s（正常 214–247 s）。
取证文档：`docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md`（含完整判据表，PR #283 合入）。

**三片修复（合理拆分，互不重叠）**：
1. PR #283（已合并）`perf(bench)`：client mock 改「流式哈希 + 丢弃」+ 守卫单测 `TestMockBenchUploadHandler_DoesNotPersistPayload`；订正 ci-merge-process.md 过期事实。
2. PR #285 `fix(bench)`：`pkg/server` 夹具**改走 RegisterRoutes 生产装配**（手搓 Handlers 与生产脱节 ⇒ 401 后还有 400「无效的文件路径」）+ 丢弃式 logger（消 8469 行 WARN）+ 存储根走 `benchStorageRoot`（Linux=/dev/shm tmpfs）+ 4 个 benchmark 补状态断言 + `make bench` 用「写 rc 文件再 exit」的纯 POSIX 写法（dash 无 pipefail；旧写法 `false | tee` 退出 0、新写法 1）+ 3 个守卫用例。
3. PR #286 `fix(telemetry)`：`slogTracer.depth` 只增不减 ⇒ 缩进 O(n²) 膨胀（CI 单 job 日志 26 MB，94% 空白、单行最多 4.3 KB）⇒ 改为按父 span 递推的无导出字段 `Span.depth`，日志 I/O 移出共享锁。

**顺带修的正确认知**：`Benchmark` **不在** master ruleset 的 7 项必检里（2026-09-15 复核：Test×2/E2E×2/Test Sub-Modules/UI E2E/**SonarQube**，`strict_required_status_checks_policy=false`）；`2026-09-13-ci-merge-process.md` 里写的 `Benchmark` 必检 + `timeout-minutes: 8` 均过期（实际 6），job 超时状态是 **cancelled** 而不是 failure。

**关键工程模式**：benchmark 夹具**不得**把 payload 写进 `b.TempDir()`（runner 系统盘）；`b.TempDir()` 是**每个 count 重建**（实测 -count=3 时目录被清空，`b.Cleanup` 每 count 跑一次）⇒ 峰值只是一次 count 的量；`go test … | tee` 会吞掉退出码（POSIX sh 无 pipefail）。

<!-- 2026-09-15 20:40:09 [01a0a4d0] -->

# Benchmark 超时治理：收尾数据与第四片（2026-09-15 完成）

**合并结果（4 片，全部 squash）**：#283 `perf(bench)`（client mock 不落盘）→ #285 `fix(bench)`（pkg/server 夹具走 RegisterProduction + pipefail + tmpfs）→ #286 `fix(telemetry)`（span 缩进泄漏）→ #287 `fix(bench)`（停滞守卫 stall guard）。

**收尾实测（run 34969491185 / job 104381990234，含全部 4 片）**：
- Benchmark job 耗时 **298 s**（基线 214–247 s；增量主要来自 pkg/server 的 4 个 benchmark 真正开始跑 = 92.6 s，其中 `BenchmarkConcurrentUploads` 因 10 并发导致 N 超调独占 ~70 s）。
- job 日志 **10.0 MB**（成功 run 曾 26 MB / flaky run 31 MB）；其中 span 行 18514（已无缩进），剩余 39868 行 WARN 来自 `pkg/server/auth.go` 的 package-level `slog.Warn`（每请求 1 条，不受 `RegisterRoutesOpts.Logger` 控制）——已列 scratchpad 跟进。

**新掌握的第二种塌陷形态（重要，别再归因错）**：修掉写盘后仍会超时（job 104376379716）：1 MiB op 恒定 **7.278–7.327 s**、4 MiB **29.4 s**（与字节数成正比、方差 ±50 ms、只影响搬数据方向），客户端与 mock 均无磁盘写入 ⇒ 是 runner 级环境 I/O 塌陷；**缩小 payload 无法规避**（N 按 ~1s/op 自适应 ⇒ 塌陷时单个 count 被拉长 ~1000 倍，比例不变）。对策 = 单次 op 停滞守卫（>2 s 立即失败 + 可操作信息），见 `pkg/client/benchmark_test.go` 的 `benchStallErr`。
