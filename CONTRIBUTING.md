# Contributing to sproxy

感谢参与。本文件是面向**人**的贡献指南；面向 AI/自动化代理的规则在 [AGENTS.md](AGENTS.md)（更细的
操作约束与踩坑记录见 `docs/superpowers/learnings/`）。两者冲突时以 AGENTS.md 为准。

## 先读这三份

| 文档 | 用途 |
|---|---|
| [README.md](README.md) | 项目定位、快速开始、常用命令 |
| [AGENTS.md](AGENTS.md) | 开发规范、门禁清单、提交与协作硬规则 |
| [RELEASING.md](RELEASING.md) | 版本与发布流程（CHANGELOG 由 release-please 生成，勿手工维护） |
| [SECURITY.md](SECURITY.md) | 安全漏洞的私下报告渠道 |

## 环境准备

- Go 1.26+；`make`（Windows 可跑 `pwsh scripts/install-make.ps1`）
- 提交前钩子需要的工具：

```bash
go install github.com/google/addlicense@latest   # SPDX 头注入（make fmt 依赖）
export PATH="$PATH:$(go env GOPATH)/bin"
make githooks                                     # 安装 pre-commit
```

- 源码、配置、脚本、Markdown **一律 UTF-8（无 BOM）**。

## 常用命令

```bash
make build          # 构建 sproxy + sclient（含格式化）
make test           # 快速单元测试（含 -race）
make test-all       # 含全部子 module
make lint           # 根 module lint
make lint-all       # 全部子 module lint
make cover-check    # 覆盖率门禁（阈值 70%，见 COVER_THRESHOLD）
make deadcode-check # 不可达符号门禁（豁免登记在 .deadcodeignore）
make check-ci       # 提交前全量检查入口
```

## 测试要求

- **纯标准库**断言（`t.Fatalf`/`t.Errorf`），不引入 testify/gomock/gomega。
- 测试服务一律绑 `127.0.0.1`（**不要**用 `0.0.0.0`/`localhost`：Windows 会弹防火墙授权）。
- 必须能在 **Windows** 上通过（平台特化测试用 `//go:build` 约束）。
- 等异步结果用条件轮询 `testutil.WaitFor`，不要写固定 `time.Sleep`（`internal/archcheck`
  的棘轮门禁会拦新增）。
- 新增功能先写**红灯测试**；声称「测试能抓 bug」前先做变异验证（把改动反向注入，确认测试真的红）。

## 提交与 PR

- 提交信息遵循 Conventional Commits：`type(scope): 中文或英文均可的简述`，多重 `-m` 写正文。
  - `fix`/`feat`/`perf`/`refactor` 等**可发布类型**会成为 CHANGELOG 条目 ⇒ subject 要写**用户可读的能力描述**；
  - 删除对外 API 用 `remove(<scope>): ...`（CHANGELOG 会归入 Removed）；
  - `chore`/`docs`/`ci`/`test`/`build`/`style` 不进 CHANGELOG。
- **不要手工改 `CHANGELOG.md` 与版本号**：由 release-please 从提交信息生成（见 RELEASING.md）。
- 分支：从 `master` 切功能分支；**不要**直接向 `master` 推送。
- PR 必须 **CI 全绿**才合并（ruleset 必检项见 AGENTS.md），合并方式 squash，合并后删除分支。
- **避免纯文档 PR**：`*.md`、`docs/**` 在 `paths-ignore` 内，不触发 CI ⇒ 必检项永远无法满足。
  文档改动请搭在代码 PR 里。

## 代码结构约定

- `cmd/` 只做参数解析、装配与输出；可复用逻辑必须落在对应的领域包（`pkg/<domain>`）。
- 界面语言：日志统一 `log/slog`；错误用 `fmt.Errorf("...: %w", err)` 包装，哨兵错误 `var ErrXxx`。
- 领域包不得反向依赖装配层；接口字段用接口类型（避免 typed-nil）。
- 新增导出符号前先确认确有外部使用者；`make deadcode-check` 会拒绝不可达符号。

## 报告问题

- 功能缺陷/使用问题：开 GitHub Issue，附**最小复现**（命令、配置、期望与现实、版本 `sproxy --version`）。
- 安全问题：**不要**开公开 Issue，见 [SECURITY.md](SECURITY.md)。
