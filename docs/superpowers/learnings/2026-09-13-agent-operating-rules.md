# 协作与实施规则（pi agent 必读）

> 来源：用户在 2026-09 的 sproxy 大规模重构（域操作抽取 / 批量族 / 分块内核 / mesh 读授权 / WebRTC 直连）
> 中**明示的操作要求与设计观点**，以及实施过程中**实测踩到的坑与验证手法**。
>
> 定位：`AGENTS.md` 只保留硬规则摘要并指向本文；**细节以本文为准**。新增经验请**追加**（注明日期与证据），
> 不要删除既有条目——它们是踩坑记录，删掉就会再踩一次。

---

## 1. 用户明示的操作要求（硬规则）

| # | 规则 | 判据 / 做法 |
|---|---|---|
| 1.1 | **中文回复**；代码、命令、日志、报错、协议字段、路径保持原文 | 所有说明/提问/进度/失败分析均中文 |
| 1.2 | 文件一律 **UTF-8 without BOM**；Windows 终端也要按 UTF-8 读，避免「文件对、终端乱码」误判 | 含中文的注释/文案/日志必须 UTF-8 |
| 1.3 | **先读后改、最小改动**：改动前读同层现有实现，沿用既有模式；不做无关重构 | 完成目标为准 |
| 1.4 | **变更须说明落点**：改了什么、在哪些文件、为什么这样改 | PR 描述 / 回复里写清 |
| 1.5 | **TDD**：先写红灯测试再实现；测试纯标准库（禁 testify 等）；测试只绑 `127.0.0.1` | 红灯必须有**失败输出**为证 |
| 1.6 | **验证先于完成**：声称完成前必须跑验证命令并给证据；未验证要说明原因与建议验证方式 | 见 §4 自检清单 |
| 1.7 | 提交只 `git add` 本任务文件（**禁 `git add -A` / `.`**）；多重 `-m`；**不加任何署名行**；标题 `type(scope): title`；body 写长中文说明 | 一次提交一件事 |
| 1.8 | **必须等 CI 全绿再合并**（本仓有 ruleset 必检 7 项）；不得提前合并 | 轮询 `gh pr checks` 到 `total≥14 且 pending=0` |
| 1.9 | **合并后删除分支**（远端 + 本地） | 本仓不会自动删 |
| 1.10 | **CI 重试只重跑失败的 job** | Benchmark job 已设 `timeout-minutes: 8`（GitHub 兜底，超时即 failure）⇒ `gh run rerun <id> --failed`；**不要**用裸 `rerun`（会把已成功的 E2E/Test/UI E2E 全部重跑，白耗 runner 且自排队尾）；需主动掐断时先 `gh api -X POST .../runs/<id>/cancel`（GitHub 无 job 级 cancel API），再 `--failed`；rerun 产生新 job id，必须动态取 |
| 1.11 | **纯文档 PR 无法合并 ⇒ 文档改动必须搭在代码 PR 里** | `paths-ignore` 含 `*.md`/`docs/**` ⇒ 不触发 CI ⇒ ruleset 必检项永不满足；且本仓 `ruleset.bypass_actors=[]` ⇒ **`--admin` 也绕不过**（实测 `GraphQL: Head branch is out of date`）。必要时给同一 PR 加一个**真实门禁/代码改动**（例：`internal/archcheck/docs_rules_test.go` 断言规则文档存在且被 `AGENTS.md` 引用） |
| 1.12 | **CI 等待期并行做下一片**；上一片合并后 `git rebase --onto origin/master <已合并提交>` 再开下一片 PR（PR 里不得夹带已合并提交） | 见 §3.10 |
| 1.13 | 推送一律走 https：`git push https://github.com/cocomhub/sproxy.git HEAD:refs/heads/<branch>`（本机 SSH 不可用） | — |
| 1.14 | **自动继续**：方案细节无须逐项确认时，直接按计划推进并在片尾报告；**发现方案缺陷要停下来讨论** | — |
| 1.15 | **Web UI 改动必须带自动化测试 + 过真实浏览器 e2e** | 用户明示：改 `web/static/**`（含嵌入式 UI）时，① 新增/改动的纯函数要有 `node --test` 单测；② 交互/渲染要有 **Playwright 真实浏览器** e2e（`web/e2e`，CI 必检项 `UI E2E Tests` 会装 chromium 后跑整套）；③ 新 JS 文件必须登记进 Makefile `web-test`（`node --check` 或 `node --test`）。`make web-test` 已挂进 ui-e2e job；门禁 **R10** (`internal/archcheck/web_assets_test.go`) 守 ①③ |

---

## 2. 用户的设计观点与已确认决策（不得擅改）

| # | 决策 | 依据/含义 |
|---|---|---|
| 2.1 | **`sync.FS` 是唯一的「远程文件操作」抽象**；`pkg/remote` 实现它；HTTP 与 mesh 只是 `ReadWriter` 的两种装载方式 | 严禁另立平行接口 |
| 2.2 | 授权轴：`mesh_readers` 条目加 **`scope: read｜write｜rw`**（缺省 `read`），**不新增独立 `mesh_writers` 列表** | 少一次配置迁移 |
| 2.3 | 顺序：**先 D-2（域操作 API）再做写批次**，避免先产出一版「改写请求」的写面 | — |
| 2.4 | **防返工**：接口形状一次性按最终形态固化（写方法先返回「未实现」错误）；延后项写成明确 TODO | P1-e 就是为此延后到有消费方时 |
| 2.5 | 本期**必须包含远端 hub**（不得假设 A 侧只连自己的 hub） | `mesh.hub_url` + 凭据 |
| 2.6 | 配置必须**实例级**（避免多租户返工）——如 ICE 的 STUN/TURN 不能只做包级全局 | `webrtc.ICEOptions` |
| 2.7 | 组织单位是**文件**不是包；子包只应是①可复用工具集合 或 ②真子领域 | 由层级/R2 门禁守护 |
| 2.8 | 重构的对外契约**零改动**要靠**机械核对 + 契约钉住测试**证明（重构前后双跑皆绿） | 见 §4 |
| 2.9 | 不做无第二消费者的抽取（如 S3/S4-C），但要**写明触发器**（什么条件下该做） | 记录而非遗忘 |
| 2.10 | 「未做/偏离/权衡」必须**写进 PR 或计划文档**，不许静默 | 例：命名偏差、语义归一化 |
| 2.11 | 计划/规格文档的更新**随代码 PR** 落地 | 规避纯文档 PR |

---

## 3. 实施经验（踩坑记录与验证手法）

### 3.1 二阶假绿：变异验证必须先断言「变异已命中」
**坑**：改完代码以为在验证测试敏感度，实际「变异」根本没匹配到目标文本 ⇒ 测试照旧全绿 ⇒ 得到**假结论**。
**做法**：变异脚本里 `assert d.count(target)==1`（命中数断言）后再跑；**变异后先 `go build`**（编译失败是弱红，说明变异破坏了代码而非触发断言）；优先做**行为级**红（如 `MakeDir: 远程写面未配置`），而不是靠删变量导致的「declared and not used」。

### 3.2 编译失败算红，但弱；要能给出行为级红
新 API 的「先红」常是编译错误。真正有说服力的是**行为断言失败**。两者都要，并在 PR 里写明是哪一种。

### 3.3 typed-nil 陷阱（本仓实测踩到）
把 nil 的**具体指针**（`*hub.HubSignaler`）赋给**接口**字段（`webrtc.Signaler`）后，接口 `!= nil` 为真 ⇒ 调用方误判「已配置」⇒ 真调用时 panic。
**做法**：接口字段就用接口类型声明「未配置 = nil」；需要判定时用守卫（本仓 `mesh.SignalerUsable` 同时排除 nil 接口与 typed nil），不要写 `s != nil`。

### 3.4 Python 与 git-bash 的 `/tmp` 不是同一个目录
Windows 上 python 看到的 `/tmp` ≠ git-bash 的 `/tmp`；跨工具传文件请用**仓库内相对路径**。

### 3.5 行尾陷阱
python 文本模式写文件会把 LF 转 CRLF ⇒ `git grep` 工作树结果带 `\r`，会让「用例名零丢失」等核对误报。改 Go 文件后**一律 `gofmt -w`**；改文档用二进制模式写。

### 3.6 Makefile 修改用 Edit 工具，不要 sed/python 多行替换
反斜杠续行 + `$$` 转义 + `{}` 嵌套极易写坏（本仓已实测失败两次，且**脚本语法错会让整段替换静默不执行**）。

### 3.7 禁用 `git stash`
本仓存在**其他分支遗留的 stash**；`git stash pop` 会弹出**别人的** WIP（已发生两次 ⇒ `UU` 冲突）。需要临时保存请用仓库内副本文件。

### 3.8 `gh pr checks` 输出为空 ≠ 全绿
两种情况：① 检查尚未挂上；② **CI 根本没触发**（纯文档 PR 的 `paths-ignore`）。判定：`gh run list --branch <b>` 为空 且 diff 全落在忽略路径 ⇒ 属②。

### 3.9 新建分支前必须 `git fetch && git checkout master && git reset --hard origin/master`
基座落后会导致 add/add 冲突**且 CI 不触发**（曾发生）。误提交到 master 的补救：`git branch <新分支>` → `git reset --hard origin/master` → `git checkout <新分支>`。

### 3.10 rebase 后必须 `--force` 推自有分支
`rebase --onto` 后是**非快进**推送；不 `--force` 会「push 被拒但 PR 已建」⇒ PR 基座错误（曾把已合并提交带进 PR）。推完要用 `gh pr diff --name-only` 核对文件数。

### 3.11 提交前必须 `export PATH="$PATH:$(go env GOPATH)/bin"`
pre-commit 需要 `golangci-lint`/`addlicense`；pre-commit 还会跑 `check-loopback`/`go vet`/`gofmt`/`make lint`。

### 3.12 门禁自身也会长期潜伏缺陷（要敢于质疑门禁）
`check-loopback` 曾把**注释**里的 `0.0.0.0` 也算违规 ⇒ 该 target 在 master 上**恒红**，而 CI 不跑它 ⇒ 长期无人发现。**做法**：门禁报错先判断是「代码违规」还是「门禁误报」；修门禁要写明理由；同时把自己的代码改成不触发（用 `net.IP.IsUnspecified()` 而非字面量比对）。

### 3.13 flake 的处置范式
给证据（CI 失败行 + 本地/重跑皆绿）→ **只放宽等待窗口、不削弱断言** → 在 PR 里披露 → 写清「真回归在放宽后的窗口下同样会红」。本仓已处置两次（shutdown accept 5s→15s；httptransport deadline 3s→15s×4 处）。

### 3.14 子模块（独立 module）相关
- 本仓有 10 个子 module；`make build-all`/`test-all`/`lint-all` 覆盖它们；
- 新增「子 module → 根 module」或反向依赖时，必须同时补 `require` + `replace`，并跑 **`GOWORK=off`** 的独立构建（`go mod tidy` 需在 `GOWORK=off` 下做一次）；
- 根 module **不能** import 无 `replace` 指向它的子 module（架构约束）⇒ 需要跨模块能力时用「接口 + 装配层注入」。

### 3.15 观测面必须与主路径同源
批量族原先**漏记删除计量、缺文件时不写审计**；分块路径曾与单次上传**各写一份 mtime/台账副作用**。**做法**：把「副作用内核」抽出共享（`recordUploadSuccess`），并用门禁钉住「只有一处实现」。

### 3.16 架构层级的实测规则
- 领域包（`pkg/files` 等）**不得 import 装配层**（`pkg/server`）；
- 子包（如 `pkg/storage/capacity`、`pkg/volume/registry`）**只被父域/装配层导入**；跨域消费走「消费者定义的窄接口」；
- 层级由**实测依赖**决定，不要凭直觉（曾把 tunnel 传输原语误判在父域之上，被门禁当场纠正 4 条）。

### 3.17 覆盖率口径陷阱
`grep "0.0%$"` 会误匹配 `80.0%`/`100.0%`；正确写法 `grep -E '[[:space:]]0\.0%$'`。（曾因此得出「74 个零覆盖函数」的错误结论，真值 39。）

### 3.18 领域错误要可判定
批量族曾靠比对**中文文案**分派错误 ⇒ 改为给 `HTTPError` 加机器可读 `Reason` 码（稳定标识，勿改字面量）。

### 3.22 前端的两道防线缺一不可（本次实测踩到）
W2 改了 UI（新增 `sclient/api/mesh.js` + 渲染函数 + e2e）后自查发现**两个真实缺口**：
① `make web-test`（`node --check` + `node --test`，206 例）**根本没接进 CI** ⇒ 前端纯函数回归只在本地可见；
② 新文件 `web/static/sclient/api/mesh.js` 漏登记进 `web-test` ⇒ 连语法检查都没有。
两者叠加 = 「前端改坏了也不红」。修法：`make web-test` 挂进必检项 `ui-e2e` job；新增门禁 **R10**
断言「`web/static` 下每个非 vendor 的 `.js` 都必须被 `web-test` 引用、`*.test.js` 必须被 `node --test` 跑、
且 `web-test` 必须挂在 `ui-e2e` job 内」——判据落在 Makefile 引用上，新增文件漏登记即红。

### 3.21 CI 重试：**只重跑失败的 job**（`--failed`）
卡死的 job 只能靠**取消整个 run** 来停（GitHub 无 job 级 cancel API），但重试时**必须**用
`gh run rerun <run-id> --failed`：裸 `rerun` 会把**已成功**的 E2E/Test/UI E2E 等分钟级 job 全部重跑，
既浪费 runner 时间也把自己排到队尾（用户明确要求避免）。取消会把「正在跑」的其它 job 一并标为
cancelled，`--failed` 恰好只重跑「失败 + 被取消」的那批，已成功的不动。

### 3.20 纯文档 PR 合不进去（本仓实测）
`gh pr merge --admin` 报 `Head branch is out of date.`：ruleset 必检项因 CI 未触发而永不满足，而
`bypass_actors=[]` 表示**连仓库管理员也没有绕过权限**。⇒ 文档必须与代码同 PR；若确实只有文档，
就在同一 PR 里加一个**有实际价值的门禁**（本仓示例：`internal/archcheck/docs_rules_test.go` 断言
「规则文档存在 + 被 AGENTS.md 引用 + 不是空壳」）——既让 CI 跑起来，又让文档不再可能被静默删除。

### 3.19 审计/日志文案也是契约
改动审计 Detail 等「对外可观察」的字符串要在 PR 里逐条列出（本仓有测试逐字断言审计行）。

---

## 4. 每次交付的自检清单（可复制执行）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"     # pre-commit 需要
gofmt -l pkg/ internal/ cmd/                 # 必须无输出
go build ./... && make build-all             # 根 + 10 子 module
make lint && make lint-all                   # 必须 0 issues（11 处）
go test -count=1 ./pkg/... ./internal/...    # 根 module 全绿
make test-all                                # 子 module 全绿
go test -race -count=1 <改动包>/...           # 竞态
make test-e2e                                # 端到端（收尾片必跑）
make check-ci                                # 全量门禁（含覆盖率门禁，收尾片必跑）
```

**四条机械核对**（重构类改动必做；基线提交 `db99de06`）：

```bash
# ① test/ 目录不得有非披露改动
git diff --stat db99de06 -- test/
# ② 用例名零丢失（want 0）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort) | grep -c '^<'
# ③ 本地 HTTP 路由表：**基线路由零丢失**（新增路由需在 PR 里列出——新增是功能，不是回归）
comm -23 <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | grep -v '/remote/' | sort) \
         <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | grep -v '/remote/' | sort)
# ↑ 输出为空 = 没有删/改任何既有路由；若输出非空必须解释或还原（tunnel 内部 /remote/* 不计入本地面）
# ④ 架构门禁
go test -count=1 ./internal/archcheck/
```

**契约钉住测试**：重构前写、重构前跑（绿）→ 重构 → 再跑（仍绿）——两次都绿才算「对外形状未变」。

---

## 5. 相关门禁与命令速查

| 事项 | 命令/位置 |
|---|---|
| 必检项（ruleset，7 条） | `Test`×2、`E2E`×2、`Test Sub-Modules`、`UI E2E Tests`、`Benchmark` |
| CI 状态 | `gh pr checks <PR>`（`total≥14 && pending==0` 才算完成） |
| 合并 | `gh pr merge <PR> --squash`（**不用 `--auto`**；纯文档 PR 才用 `--admin`） |
| 删分支 | `git push <url> --delete <branch>` + `git branch -D <branch>` |
| 门禁清单 | `internal/archcheck/`：R1 分层方向 / R2 子包可见性 / R3 新包登记 / R4 领域包不得导入装配层 / R5 全表化 / R6 子 module 边界 / R7 重复实现 / R9 规则文档不腐烂 / R10 前端 JS 全覆盖（被 `web-test` 引用 + 测试被 `node --test` 跑 + `web-test` 挂 CI）/ `Managed∖Levels` 断言 / xfer Send 原子性 / 上传副作用单一实现 |
| 计划与规格 | `docs/superpowers/plans/`、`docs/superpowers/specs/`（**随代码 PR 更新**） |
| 既有流程文档 | `docs/superpowers/learnings/2026-09-13-ci-merge-process.md`（CI/合并细节） |
