# 2026-08-23 version / meta / buildinfo 设计

状态：已评审通过（用户 16:27 approve）。

## 背景与动机

- `cmd/sclient/main.go`、`cmd/sproxy/main.go` 的 `Version`/`BuildAt` 由 Makefile `GO_LDFLAGS` **从未注入**（sproxy `GO_LDFLAGS` 为空），二者恒为 `"dev"`/`"unknown"`，无法判断服务版本。
- sclient `version` 顶层命令**用途混淆**：既是「程序二进制版本」又是「文件版本管理」（`version list|restore|delete`）。
- 希望把程序版本能力做成**通用可复用库**（cocomhub 其他项目都有类似需求），并支持 dirty-info（未提交变更追踪）。
- 允许破坏性调整（移除旧命令），**不写 README/CHANGELOG**，破坏性变更只在 commit message 标注（`!`）。

## 决策汇总

1. sclient 新增 **`meta`** 命令聚合文件元信息 + 文件版本管理；**移除** `version list|restore|delete`。
2. `version` 顶层命令**完全让给程序二进制版本**（`sclient version [--json]` + `sclient version dirty-info`）。
3. sclient `stat` 改造：`stat` 无参 = 本地 client 状态；`stat server` = 远端服务状态；**移除 `stat <file>` 文件元信息**，统一到 `meta`。
4. sproxy 保留 `sproxy --version`，新增 `sproxy version` 子命令（同一输出）。
5. 新建**独立仓库** `github.com/cocomhub/buildinfo` 纯库（无 embed），提供 `Info`/`NewVersionCmd`/`PrintVersion`/`PrintVersionJSON`。
6. sproxy 根模块新增 `internal/buildmeta` 单点 embed 共享 dirty_info（`VERSION_DIR ?= internal/build`，一份文件 embed 一次，sclient/sproxy 两 cmd 共享）。
7. Makefile `SKIP_VERSION ?= false`（默认生成 dirty_info）。

## 命令树

```
# sclient
sclient version                      # 程序二进制版本（+ 配置摘要，对齐现 RunE）
sclient version --json          # 输出 JSON（定死支持 --json；-f 自定义格式不做，YAGNI）
sclient version dirty-info            # 输出 DirtyInfo（来自 internal/buildmeta embed）
sclient meta <file>                 # 文件元信息 + 版本历史摘要
sclient meta version list <file>              # = 现 version list
sclient meta version restore <file> <id>     # = 现 version restore
sclient meta version delete <file> <id>      # = 现 version delete
sclient stat                        # 无参 = 本地 client 状态
sclient stat server                 # 远端服务状态（healthz/stats/带宽/连通）

# sproxy
sproxy --version                  # 保留
sproxy version                  # 新增（等价 --version）
```

## 文件/模块变更（sproxy 仓库）

| 路径 | 变更 |
|---|---|
| `internal/buildmeta/buildmeta.go` | 新增。`//go:embed build/dirty_info.txt` → `DirtyInfo`/`DirtyID` 导出变量 |
| `internal/build/build/` | Makefile `prepare` 生成 `dirty_info.txt`（`VERSION_DIR ?= internal/build`，加入 `.gitignore`） |
| `Makefile` | `VERSION_DIR ?= internal/build`；`SKIP_VERSION ?= false` |
| `cmd/sclient/version.go` | 重写：`NewCmdVersion` 调用 `buildinfo.NewVersionCmd(...)`，入参 `Info{...}` + `DirtyInfo` 来自 `internal/buildmeta`；移除 `NewCmdVersionList/Restore/Delete` |
| `cmd/sclient/meta.go` | 新增：`meta <file>` + `meta version list|restore|delete`（迁移自 version.go） |
| `cmd/sclient/stats.go` | 改造：无参=本地态、`server`=服务态；移除文件元信息 |
| `cmd/sclient/root.go` | 注册 `meta`；注册 `version dirty-info` |
| `cmd/sproxy/root.go` | 新增 `version` 子命令（复用 `buildinfo.PrintVersion`） |
| `cmd/sproxy/root_test.go` | 保留 `--version` 测试；新增 `version` 子命令测试 |
| `cmd/sclient/version_test.go` | 重写程序版本测试；`meta`/`stat server` 测试 |
| `go.work` | 加入本地 buildinfo（`replace` 指向本地 clone，联调用） |

## 规格自检记录

- **占位符**：无 TODO/待定。`-f` 自定义格式已定死为不做（YAGNI），保留 `--json`。
- **内部一致性**：命令树、决策汇总、文件变更三处 `version`/`meta`/`stat`/`buildinfo`/`buildmeta` 命名一致。
- **范围检查**：聚焦一个可执行计划（buildinfo 仓库建立 + sproxy 三个命令改造），未再拆分。
- **歧义检查**："buildinfo"（独立仓库）与 "buildmeta"（sproxy 根模块）已明确区分；"dirty_info 单点"已确认即单份文件。

## 外部依赖

- 新增依赖 `github.com/cocomhub/buildinfo`（sproxy 根 module、sclient、sproxy 三处 require；本地开发用 `replace` 指向本地 clone）。
- **仅标准库 + yaml.v3 + 既有依赖**；buildinfo 自身零第三方依赖（除 cobra，它是 cobra 命令工厂）。

## 注入与字段（buildinfo.Info）

- `Info{Version, BuiltAt, CommitID, Branch, DirtyID, DirtyInfo, ReleaseURL, GoVersion(GOOS/GOARCH 自动)}`
- Makefile `-X github.com/cocomhub/buildinfo.Version=...` 等（对根 module 与两个 cmd module 各自注入对应符号）。
- `internal/buildmeta.DirtyID` = `md5(DirtyInfo)[:10]`，空 = `"clean"`。

## dirty_info 单点共享（验证）

- **已做实体验证**：`//go:embed internal/build/dirty_info.txt`（同级子目录）编译通过与运行时输出正常。
- embed 无法跨仓库引用（`..` 禁止），因此独立仓库 `buildinfo` **不 embed**；文件在 sproxy 根模块 `internal/build` 生成一份、`internal/buildmeta` embed 一次，两个 cmd 共享导出变量。

## 测试

- `-race -count=1`，127.0.0.1 回环，纯标准库测试（无 testify/gomock）。
- `--config` 指向临时文件，不用 `--server` 防本地配置污染。
- 破坏性变更：更新/新增 `cmd_test.go` 命令树断言；`meta test` 用 mock server 验证 `GET /api/versions` 等。

## 非目标

- cocom / download-manager **不迁移**到 buildinfo（各自既有 `pkg/version`/内联）；仅文档化，未来可选（YAGNI）。
- 不写 README/CHANGELOG；破坏性变更在 commit message 用 `!` 标注。
