# 开发工具（dev tools）

> 本文登记**已入库**的开发/运维工具，以及「临时程序何时该整合入库」的判据。
> 历史一次性脚本留在 `build/`（gitignored），不入库。

## 一、已入库工具

| 工具 | 用途 | 运行方式 |
| --- | --- | --- |
| `tools/e2echeck` | 对**运行中**的 sproxy 实例逐条打真实 HTTP 做**全能力面冒烟**（公开面/未认证面/上传→下载→校验/分享/批量/分块上传/凭据…）。断言原则：任何路由不得 5xx（除登记例外）、关键能力必须真工作、有业务错误的端点允许 4xx 但不允许「接线错误 500」。内置 **`-seed` 夹具**：把与默认 `-ak/-sk/-skid` 匹配的占位凭据写入 `<dir>/anonymous/meta/credentials.json`，便于本地起一个 `storage_root=<dir>` 的实例后直接冒烟（原 `build/e2e/seed`） | `go run ./tools/e2echeck -seed <storage_root>` 后 `go run ./tools/e2echeck -addr=https://127.0.0.1:18084 -ak=<AK> -sk=<SK> -skid=<SK条目ID>`；或 `make build-dev-tools` 后跑 `build/bin/e2echeck` |
| `tools/uploadperf` | 上传吞吐压测（`plain` 单请求 / `chunked` 分块 + 并发），并支持清理测试目录（`ls`/`rm`/`rmdir`）；绕开业务层只压 sproxy 上传路径 | `go run ./tools/uploadperf -mode=chunked -server=https://127.0.0.1:18084 -chunk=4194304 -conc=4 -ak=.. -sk=.. -skid=..` |
| `scripts/mutcheck.sh` | **变异验证骨架**（硬规则 5）：施加一处真实变异 ⇒ 断言指定测试**变红** ⇒ **无论成败自动复原**源文件。退出码：0=变异被捕获 / 1=**未被捕获（测试缺口）** / 2=参数错 / 3=锚点未命中或不唯一 / 4=缺依赖 | `make mutcheck ARGS="-file <f> -old '<a>' -new '<b>' -pkg <pkg> -run <regex>"` |
| `scripts/regression-audit.sh` | **回归审计**（硬规则 1.17）：用 `git show <commit> -U0` 抽出**被删除的导出符号**与**能力相关字段/档位**，并实测「裸默认池构造点 / `CloseIdleConnections` 调用点」当前状态，要求逐条给出行级判定 | `make regression-audit`（默认 origin/master 最近 12 个提交）或 `ARGS="--current"` / `ARGS="<sha>…"` |

> `make test-e2e` 会**自建进程 + 临时存储**跑端到端；`tools/e2echeck` 打的是你手动起好、带真实配置/卷/凭据的实例 —— 两者互补。

## 二、判据：临时程序何时整合入库

| 情形 | 处置 |
| --- | --- |
| **跨任务会重复使用** + 不绑定某一次具体改动 + 不依赖个人环境 | **整合入库**：Go ⇒ `tools/<name>/`（+ `.notestignore` 登记 + Makefile target）；bash ⇒ `scripts/<name>.sh`（+ 可选 `*_test.sh` 夹具测试 + `make test-<name>`） |
| 一次性 codemod（`patch_*.py`、`mig_*.py`、`split_go_file.py`…） | 不入库（结果已在源码里；脚本只是当时的手段） |
| 已执行完的运维/迁移脚本（容器名规范化、blob 搬迁…） | 不入库（一次性动作；若流程要复用，改写为 sclient/服务端子命令才是正解） |
| **门禁基线的再生成器** | **不要入库**——门禁自身已内建：`ARCHCHECK_WRITE_BASELINE=1 go test ./internal/archcheck/ -run <TestXxx>`（R14/R18/R57 同款）。仓库外的副本会与门禁漂移 |
| 代码片段快照（`*_snippet.go.txt`、`*_body.md`、`commit-msg-*.txt`、PR body 草稿） | 不入库（代码已在源文件中，快照必然漂移；PR body 归档在 GitHub） |

## 三、硬约束

1. **禁止真实数据**（硬规则 21/21b）：工具的目标地址/凭据一律**运行期传入**（CLI flag 或环境变量），
   默认值只能是占位符（如 `127.0.0.1`、`ak-0000…`）。新增工具前先跑一遍真实数据模式扫描。
2. **工具也要走既有基座**：HTTP 一律用 `pkg/httpc`（`NewHTTPClient` 取裸 client、`New`+`Do` 要编排），
   不要新造裸 `&http.Client{}`（`tools/` 虽在 R57 豁免内，但漂移会随复制扩散）。
3. **新增 tools 包必须登记 `.notestignore`**（并写明理由），否则 `make notest` 报红。

## 四、分支 / 工作区清理流程（2026-11-08 实测）

**判据必须用 GitHub PR 记录，不要用 commit subject。** squash 合并时常改写 subject ⇒ subject 匹配会产生
**≈98% 假阴性**（实测 50 个分支只匹配 1 个）；内容级指标也不可靠（master 后来演进会让老分支的「新增行」自然消失）。

1. 拉全部 PR 的 head/state：`gh pr list --state all --limit 1200 --json number,headRefName,state,mergedAt > build/prs.json`
   ——**limit 必须 ≥ 仓库 PR 总数**，否则老分支会被误判成「从未提 PR」。
2. 与 `git branch` 求**交集**（只处理本地真实存在的分支）后分类：
   | 分类 | 判据 | 处置 |
   |---|---|---|
   | 已交付 | 该 head 有 `mergedAt` | 可删 |
   | 进行中 | `state=OPEN` | **保留** |
   | 被取代 | `state=CLOSED` 且未合并（核对后继 PR） | 可删 |
   | 本地探索 | 无任何 PR | **需人工判定** |
3. 删除前确认未被 worktree 占用：`git worktree list --porcelain`。占用时先 `git worktree remove <dir>`
   ——**移除前必须确认工作区干净**。
4. **遇到遗留未提交改动：不要 `rm -rf`，也不要 `--no-verify` 强行提交。** 按硬规则先让 pre-commit 通过
   （通常只是 lint/格式），再用**合法 Conventional 类型**提交到该分支保全，例如
   `test(<scope>): 保全 <来源> 的遗留改动（未验证 WIP）`——注意 `wip: …` **不是**合法类型，commit-msg 钩子会拒绝。
   提交后移除 worktree，工作仍在分支里（可复查、可丢弃）。
5. 收尾：`git worktree prune` 清掉失效登记。

### 实测踩到的 3 个陷阱

- **CRLF 静默跳过**：python 在 Windows 写出的清单是 CRLF ⇒ `while read -r b` 得到的分支名尾部会带 `\r`（回车符）⇒ `git rev-parse` 全部失配、循环体形同空转（表现为「已删除 0 个」，且不报错）。用 `tr -d '\r'` 过滤，或在写出时用 `open(..., newline='')`。

- **遍历对象搞错**：要遍历**本地分支**并查 PR，而不是遍历 PR 记录去查分支（后者里绝大多数 ref 本地并不存在，
  只会静默跳过，白跑一轮）。
- **MSYS 路径转换**：Git-Bash 下 `git show origin/master:<path>` 会被改写成 Windows 路径（`/`→`\`、`:`→`;`）
  而报 `ambiguous argument` ⇒ 加 `MSYS_NO_PATHCONV=1`，或用 `git cat-file`。

