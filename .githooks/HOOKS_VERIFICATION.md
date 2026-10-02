# .githooks 验证场景清单（回归锁定基准）

> 本文档记录 `.githooks/` 下每个脚本的**已验证场景**与**验证命令**。
> **改动任何 hook 脚本后，必须跑通本文档所列全部相关场景**（新增场景同步补录），
> 防止改动引入回归。执行环境：Git Bash（Windows）/ POSIX sh。
>
> 验证方法速记：所有场景都通过 **staged 文件 + 手动执行 hook** 完成，不改动真实提交；
> staged 后立即 restore 恢复工作区（见各节「恢复」）。
>
> 日志落盘约定：pre-commit → `build/hooks/pre-commit.log`；pre-push → `build/hooks/pre-push.log`；
> 每次运行覆盖。hook 顶部与结尾均打印日志绝对路径。

---

## 通用：模块归属判定（`find_mod_root`）

被 pre-commit 第 3/4 步（go vet / golangci-lint 按 module 定位）共用。
**期望：** 给定文件相对路径，返回所属 module 根（向上找 `go.mod`，找不到 → `.`）。

| 文件路径（样例） | 期望 module 根 |
|---|---|
| `pkg/server/foo.go` | `.`（根 module） |
| `cmd/sproxy/root.go` | `cmd/sproxy` |
| `cmd/sclient/cd.go` | `cmd/sclient` |
| `cmd/sproxy-mcp/main.go` | `cmd/sproxy-mcp` |
| `pkg/baidupcs/b.go` | `pkg/baidupcs` |
| `pkg/authn/ext/oidcldap/x.go` | `pkg/authn/ext/oidcldap` |
| `pkg/tunnel/hub/ext/kad/x.go` | `pkg/tunnel/hub/ext/kad` |
| `pkg/tunnel/mesh/x.go` | `pkg/tunnel/mesh` |
| `pkg/tunnel/xfer/ext/ws/x.go` | `pkg/tunnel/xfer/ext/ws` |
| `pkg/tunnel/xfer/ext/quic/x.go` | `pkg/tunnel/xfer/ext/quic` |
| `pkg/tunnel/xfer/ext/grpc/x.go` | `pkg/tunnel/xfer/ext/grpc` |
| `pkg/volume/ext/pikpak/x.go` | `pkg/volume/ext/pikpak` |
| `pkg/volume/ext/s3/x.go` | `pkg/volume/ext/s3` |
| `web/e2e/foo.go` | `web/e2e`（独立 go.mod） |
| 根目录 `main.go` | `.` |

**验证命令：**

```sh
find_mod_root() {
  d=$1
  while [ "$d" != "." ] && [ "$d" != "/" ]; do
    if [ -f "$d/go.mod" ]; then echo "$d"; return; fi
    d=$(dirname "$d")
  done
  echo "."
}
# 抽查：
find_mod_root pkg/server      # 期望 .
find_mod_root cmd/sproxy      # 期望 cmd/sproxy
find_mod_root pkg/tunnel/xfer/ext/ws  # 期望 pkg/tunnel/xfer/ext/ws
```

---

## pre-commit（增量轻量检查，仅本次 commit 涉及文件）

**场景 A — 通过路径**

| # | 场景 | staged 内容 | 期望 |
|---|---|---|---|
| A1 | 主 module go 文件 | 修改 `pkg/server/*_test.go`（合法格式 + SPDX 头） | 5 步全绿，exit 0 |
| A2 | 子 module go 文件 | 修改 `cmd/sproxy/root.go`（合法） | vet/lint 在 `cmd/sproxy` 内跑，exit 0 |
| A3 | 嵌套子 module go 文件 | 修改 `pkg/tunnel/xfer/ext/ws/*.go`（合法） | 在 ws module 内跑，exit 0 |
| A4 | 跨 module 混合 | 同时 staged：主 module + `cmd/sproxy` + ws 各一个文件 | 按 module 分组各跑一次，exit 0 |
| A5 | 纯文档提交 | staged 仅 `.md`（无 go 文件） | 提示「无 go 文件变更」快速放行，exit 0 |

**场景 B — 拦截路径（每步独立验证；gofmt/SPDX 文件级天然覆盖任意 module，vet/lint 按 module 分组，loopback 文件级）**

| # | 触发步 | staged 内容 | 期望 |
|---|---|---|---|
| B1 | 第 1 步 gofmt（主） | 根/主 module 新建格式不合格 go 文件（如 `func F(a int,b int)`） | exit 1，日志列文件名 |
| B1b | 第 1 步 gofmt（子） | `cmd/sproxy/hook_probe_bad_fmt.go`（`package main`，格式不合格） | exit 1，日志列文件名 |
| B2 | 第 2 步 SPDX（主） | gofmt 合格但**无 SPDX 头**的 go 文件 | exit 1，日志列文件名 |
| B2b | 第 2 步 SPDX（子） | `cmd/sproxy/hook_probe_nospdx.go`（gofmt 合格、无 SPDX） | exit 1，日志列文件名 |
| B3 | 第 5 步 loopback（主） | 真实 `pkg/server` 测试文件注入 `net.Listen("tcp","0.0.0.0:8080")`（带 SPDX + 合法格式） | exit 1，日志含违规行 |
| B3b | 第 5 步 loopback（子） | `cmd/sproxy/hook_probe_lb_test.go`（`package main`，带 SPDX+gofmt 合格+`import net`，监听 `0.0.0.0:9090`） | exit 1，日志含违规行（**注意：注入文件必须已 `import net`，否则先被 vet 拦截**） |
| B5 | 第 3 步 vet 失败（主） | `pkg/server/hook_probe_vet_test.go`：`fmt.Printf("%d", "bad")`（printf 格式错误） | exit 1，日志含「go vet 失败」+ vet 输出 |
| B5c | 第 3 步 vet 失败（子） | `cmd/sproxy/hook_probe_vet2_test.go`：同上格式错误 | exit 1，日志含「go vet 失败: cmd/sproxy/.」 |
| B6 | 第 4 步 lint 失败（子） | `cmd/sproxy/hook_probe_lint.go`：`import "crypto/md5"`（gosec G501/G401 弱哈希） | exit 1，日志含「golangci-lint 失败」+ gosec 输出 |

> **set -e 陷阱（2026-10-02 实测修复）**：脚本头 `set -e` 会让 `vet_out=$(cd … && go vet … 2>&1)` 这类**赋值式命令替换**在 vet/lint 失败（rc≠0）时**立即静默退出**——「go vet 失败」say 与日志落盘不会执行，日志只停在 `[3/5]`。修复：改为 `if ! vet_out=$(…); then say …; fi`（if 条件内的命令失败不触发 set -e）。**回归判定：vet/lint 失败场景的日志必须含错误定位行（不只是停在步骤标题）。**

**验证命令（A1 通过路径示例）：**

```sh
# A1：staged 一个合法主 module 文件
cp pkg/server/accept_retry_test.go /tmp/a1.go
printf '\n// A1 probe\n' >> pkg/server/accept_retry_test.go
git add pkg/server/accept_retry_test.go
./.githooks/pre-commit; echo "exit=$?"   # 期望 exit=0
# 恢复
git restore --staged pkg/server/accept_retry_test.go
git checkout -- pkg/server/accept_retry_test.go
```

**验证命令（B2 SPDX 拦截示例）：**

```sh
cat > hook_spdx_test.go <<'EOF'
package hookprobe

func Add(a, b int) int { return a + b }
EOF
gofmt -w hook_spdx_test.go && git add hook_spdx_test.go
./.githooks/pre-commit; echo "exit=$?"   # 期望 exit=1
grep "缺少 SPDX 头的文件" build/hooks/pre-commit.log
git restore --staged hook_spdx_test.go && rm -f hook_spdx_test.go
```

**验证命令（B5 vet 失败 / B6 lint 失败 —— set -e 回归关键场景）：**

```sh
# B5 vet 失败（主 module）：printf 格式错误
cat > pkg/server/hook_probe_vet_test.go <<'EOF'
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"testing"
)

func TestHookProbeVet(t *testing.T) {
	fmt.Printf("%d\n", "not-an-int")
}
EOF
gofmt -w pkg/server/hook_probe_vet_test.go && git add pkg/server/hook_probe_vet_test.go
./.githooks/pre-commit; echo "exit=$?"   # 期望 exit=1
grep "go vet 失败" build/hooks/pre-commit.log   # 期望有该行 + printf 定位行
git restore --staged pkg/server/hook_probe_vet_test.go && rm -f pkg/server/hook_probe_vet_test.go

# B6 lint 失败（子 module）：gosec G501/G401（crypto/md5）
cat > cmd/sproxy/hook_probe_lint.go <<'EOF'
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/md5"
	"fmt"
)

func HookProbeMD5(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s)))
}
EOF
gofmt -w cmd/sproxy/hook_probe_lint.go && git add cmd/sproxy/hook_probe_lint.go
./.githooks/pre-commit; echo "exit=$?"   # 期望 exit=1
grep "golangci-lint 失败" build/hooks/pre-commit.log   # 期望有该行 + G501/G401 定位
git restore --staged cmd/sproxy/hook_probe_lint.go && rm -f cmd/sproxy/hook_probe_lint.go
```

**验证命令（B3/B3b loopback 拦截示例）：**

```sh
cp pkg/server/accept_retry_test.go /tmp/b3.go
cat >> pkg/server/accept_retry_test.go <<'EOF'

func TestLoopbackProbe(t *testing.T) {
	net.Listen("tcp", "0.0.0.0:8080")
}
EOF
gofmt -w pkg/server/accept_retry_test.go && git add pkg/server/accept_retry_test.go
./.githooks/pre-commit; echo "exit=$?"   # 期望 exit=1
grep "0.0.0.0" build/hooks/pre-commit.log
cp /tmp/b3.go pkg/server/accept_retry_test.go   # 恢复原内容
git restore --staged pkg/server/accept_retry_test.go && git checkout -- pkg/server/accept_retry_test.go
```

---

## pre-push（push 前全量门禁：fmt-all → lint-all → check-ci）

| # | 场景 | stdin 输入 | 期望 |
|---|---|---|---|
| C1 | tag 推送 | `refs/tags/v9.9.9 <sha> refs/tags/v9.9.9 <sha>` | 跳过全量，exit 0 |
| C2 | 空 push / 无 stdin | （空） | 跳过全量，exit 0 |
| C3 | 分支推送（mock make 成功） | `refs/heads/x <sha> refs/heads/x <sha>` | 按序调用 fmt-all → lint-all → check-ci，exit 0，日志落盘完整 |
| C4 | 分支推送（mock 第 1 步失败） | 同上，make 模拟 exit 1（多行错误输出） | exit 1，**日志必须含 `✗ make fmt-all 失败` + make 完整错误输出**（回归判定：不能只停在 `[1/3]` 步骤标题） |
| C4b | 分支推送（mock 第 2 步失败） | 同上，fmt-all 成功、lint-all 失败 | exit 1，日志含 `✗ make lint-all 失败` + 错误输出 |
| C5 | 分支推送（真实 lint-all） | 同上（mock 外真实跑 `make lint-all`） | 全部 module 0 issues，exit 0 |
| C6 | check-ci 内 deadcode 失败 | 真实 make（mock 前三步）+ 注入不可达函数（有调用者但调用链不可达，避免被 unused 先拦） | exit 1，日志含 `FAIL: unreachable symbols found` + 函数列表 |
| C7 | check-ci 内 notest 失败 | 真实 make + 新建无测试文件的包 | exit 1，日志含 `FAIL: <pkg> has no test files` + 包路径 |
| C8 | check-ci 内 cover-check 失败 | 临时 `COVER_THRESHOLD ?= 100` 跑 `make cover-check` | exit 1，输出含 `FAIL: coverage X% < threshold 100%`（测完恢复 70） |

> **set -e 陷阱（2026-10-02 实测修复，与 pre-commit 同型）**：`make_out=$(make … 2>&1); make_rc=$?`
> 赋值式命令替换在 make 失败（rc≠0）时被脚本头 `set -e` **立即静默退出**——`✗` 提示与 make 错误
> 输出不会落盘，日志只停在 `[N/3]` 步骤标题。修复：改为 `if ! make_out=$(make … 2>&1); then say …; fi`。
> **回归判定：任一 make 失败时 pre-push.log 必须含 `✗` 行 + make 错误输出（不只停在步骤标题）。**

**验证命令（C1/C2 跳过 + C3/C4 mock）：**

```sh
# C1
printf 'refs/tags/v9.9.9 aaaa refs/tags/v9.9.9 bbbb\n' | ./.githooks/pre-push; echo "exit=$?"
# C2
printf '' | ./.githooks/pre-push; echo "exit=$?"
# C4：mock 第 1 步失败（错误输出须完整落盘）
mkdir -p /tmp/mk && cat > /tmp/mk/make <<'EOF'
#!/bin/sh
echo "MOCK make $@"
echo "  cmd/sproxy/foo.go:10:2: unreachable func"
echo "  make: *** [Makefile:569: check-ci] Error 1"
exit 1
EOF
chmod +x /tmp/mk/make
PATH="/tmp/mk:$PATH" sh -c 'printf "refs/heads/x aaa refs/heads/x bbb\n" | ./.githooks/pre-push'; echo "exit=$?"
cat build/hooks/pre-push.log     # 期望含 ✗ + MOCK 错误三行
rm -rf /tmp/mk
```

**验证命令（C6 deadcode 失败 / C7 notest 失败 / C8 cover-check 失败）：**

```sh
# C6：deadcode 探针（有调用者但整体不可达，绕过 unused 先拦）
cat > cmd/sproxy/hook_dc.go <<'EOF'
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

func hookDcCaller() { hookDcProbe() }
func hookDcProbe() int { return 42 }
EOF
gofmt -w cmd/sproxy/hook_dc.go && git add cmd/sproxy/hook_dc.go
make deadcode-check; echo "exit=$?"   # 期望 exit 2，输出 FAIL + hookDc 列表
git restore --staged cmd/sproxy/hook_dc.go && rm -f cmd/sproxy/hook_dc.go

# C7：notest 探针（新建无测试的包）
mkdir -p pkg/hook_nopkg && printf 'package hooknopkg\nfunc F() int { return 1 }\n' > pkg/hook_nopkg/foo.go
make notest; echo "exit=$?"   # 期望 exit 2，输出 FAIL + pkg 路径
rm -rf pkg/hook_nopkg

# C8：cover-check 失败（临时阈值 100）
cp Makefile /tmp/Makefile.bak && sed -i 's/COVER_THRESHOLD ?= 70/COVER_THRESHOLD ?= 100/' Makefile
make cover-check; echo "exit=$?"   # 期望 exit 2，输出 FAIL: coverage X% < threshold 100%
cp /tmp/Makefile.bak Makefile
```

> 注意：**C5–C8 为真实全量验证**，耗时数分钟，建议在改动 pre-push / Makefile 时最后跑；
> 快速迭代用 C1–C4 mock 即可。

---

## commit-msg（提交信息规范，Conventional Commits 强制 scope）

| # | 场景 | 提交信息 subject | 期望 |
|---|---|---|---|
| D1 | 合法 | `feat(secret): 加密卷` | 通过 |
| D2 | 合法多 scope | `fix(server,client): 修复` | 通过 |
| D3 | 破坏性变更 | `feat(secret)!: breaking` | 通过 |
| D4 | 缺 scope | `fix: 修复` | 拦截 |
| D5 | 非法 type | `whatever(x): title` | 拦截 |
| D6 | 豁免（bot/自动生成） | `Merge ...` / `chore: release master` / `fixup! ...` | 通过 |

**验证命令（D1 合法 / D4 缺 scope / D5 非法 type 抽查）：**

```sh
# 注：commit-msg 从 $1 读消息文件；必须用真实临时文件（Git Bash 下 /dev/stdin 管道
# 会因 stdin 已消费而读取异常，导致误判 exit 0）。
printf 'feat(secret): 加密卷\n' > /tmp/d1.txt && ./.githooks/commit-msg /tmp/d1.txt; echo "exit=$?"  # 期望 0
printf 'fix: 修复\n'        > /tmp/d4.txt && ./.githooks/commit-msg /tmp/d4.txt; echo "exit=$?"  # 期望 1（缺 scope）
printf 'whatever(x): t\n'   > /tmp/d5.txt && ./.githooks/commit-msg /tmp/d5.txt; echo "exit=$?"  # 期望 1（非法 type）
printf 'feat(secret)!: b\n' > /tmp/d3.txt && ./.githooks/commit-msg /tmp/d3.txt; echo "exit=$?"  # 期望 0（破坏性）
rm -f /tmp/d*.txt
```

---

## 日志落盘核对

每次 pre-commit / pre-push 运行后：

```sh
ls -la build/hooks/                # pre-commit.log / pre-push.log 存在
cat build/hooks/pre-commit.log     # 含：检查标题、日志路径、各步结果、通过/失败结论
```

**期望：** 日志每次运行**覆盖**（`: > "$LOG_FILE"` 开头清空）；失败场景日志含具体定位
（文件名 / 违规行 / make 失败输出）；hook 顶部与结尾均打印日志绝对路径。

---

## 提交纪律（配合 hooks 的硬规则）

- **绝对禁止 `git commit --no-verify` / `git push --no-verify` 跳过检查**（AGENTS.md 协作与流程硬规则、
  sproxy/AGENTS.md 第 6 条）。
- 出现**环境问题或脚本故障时直接修复环境/脚本**，不得以「绕过后 CI 会兜底」为由跳过。
- pre-commit 为增量轻量（秒级），pre-push 为全量（fmt-all/lint-all/check-ci）；
  CI 的 Lint/Test job 仍是等价全量兜底。
