<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 隐藏构建器（stealth-builder）设计规格

- 日期：2026-09-22
- 状态：草案（待用户审查）
- 关联：sproxy 开源主仓 `github.com/cocomhub/sproxy`（本规格不要求主仓任何改动）

## 1. 背景与目标

sproxy 是开源项目，二进制内包含大量可识别的品牌信息（CLI 命令 `sproxy`/`sclient`、
版本输出、`github.com/cocomhub/sproxy` 路径、Web UI 标题、HTTP realm、配置路径
`~/.config/sproxy`、日志前缀等）。部分用户希望在**自用部署**场景下交付一个
「看不出是 sproxy」的隐藏二进制：

- 静态分析（strings/grep/杀软）无法把它和 sproxy 关联起来；
- 不暴露版本号、模块路径、构建信息；
- 不以「子命令齐全的 CLI」形态出现——希望做成「配置写死、直接运行」的守护程序。

约束（用户明确）：

- **主仓零改动**：开源主仓保持纯净，不为隐藏需求添加接缝。
- **独立隐藏仓库**：隐藏构建能力独立成仓，以「混淆器工具」形态维护。
- **全链路隐藏**：静态（garble 混淆 + 字符串替换 + 移除版本/命令声明）+
  运行时（品牌字符串、command 名、日志前缀、配置路径）都要处理；
  网络协议指纹（HTTP 头、路由路径、端口）由用户另一项工程单独负责，不在本规格范围。
- **开源源码不混淆**：开源的 Go 源码本身是公开的，混淆只作用于构建产物，
  目标是让**单个二进制**无法被快速识别，不追求防专业逆向。

## 2. 命名

- 隐藏仓库：`github.com/cocomhub/stealth-builder`（独立 git 仓库）。
- 混淆器：`stealth-build`（CLI 工具，本身是普通 Go 程序，不混淆）。
- 隐藏二进制代号示例：systemd 风格名，如 `prismd`（服务端）/ `bridge`（客户端）。
  具体代号由隐藏部署者通过配置决定，工具不硬编码。

## 3. 架构总览

```
主仓 tag（如 v0.17.0）
    │  git archive --remote / 本地 clone + checkout tag
    ▼
临时源码树 /tmp/stealth-src-<hash>
    │  stealth-build obfuscate（源码改写层，见 §5）
    ▼
改写后的源码树（品牌串已替换、版本/命令声明已剔除、buildinfo 已断）
    │  stealth-build build（构建层，见 §6）
    ▼
garble 混淆构建（-literals -tiny -seed 可配）
    ▼
隐藏二进制 prismd / bridge
```

两个阶段可以分开调用：`obfuscate` 输出改写后的源码树（供检查/调试），
`build` 一次性完成 改写 → 构建 → 冒烟验证。

## 4. 用户配置（隐藏清单）

隐藏仓库内置 `stealth.yaml` 作为默认配置，支持覆盖：

```yaml
# stealth-build.yaml（示例）
source:
  repo: https://github.com/cocomhub/sproxy
  tag: v0.17.0            # 构建时拉取该 tag
target:
  server_binary: prismd    # 服务端二进制名
  client_binary: bridge    # 固定操作客户端二进制名
  brand: prism             # 品牌代号（Web 标题/描述/日志前缀的显示名）
garble:
  literals: true
  tiny: true
  seed: random             # random | fixed:<hex>
bridge:
  enabled: true            # 单操作固定配置
  action: download
  server_url: https://10.0.0.8:18083
  filename: snapshot.tar.gz
  output: /var/backup/snapshot.tar.gz
```

## 5. 源码改写层（obfuscate）

对从主仓拉取的源码树做**纯文本/源码级**改写，产出可独立构建的改写树。
原则：不修改任何业务逻辑；所有替换集中在「身份标识」上。

### 5.1 品牌字符串替换表（核心）

维护一张 `old → new` 映射表，覆盖以下类别。所有替换均限定在**非 import 路径**内
（`github.com/cocomhub/sproxy` 的 import 与 module path 必须保留，见 §7 例外）。

| 类别 | 原文示例 | 替换为 |
|------|----------|--------|
| CLI Use 名 | `Use: "sproxy"` / `Use: "sclient"` | `Use: "prismd"` / `Use: "bridge"` |
| CLI 描述 | `批量删除 sproxy 服务端上的文件。` 等 cobra Long 文案 | 中性措辞（按映射逐条替换 `sproxy` → `prism`） |
| Web UI 标题 | `<title>sproxy 文件管理</title>`、`<h1>sproxy 文件管理</h1>` | `<title>prism 文件管理</title>` |
| HTTP realm | `Basic realm="sproxy"` | `Basic realm="prism"` |
| 配置/缓存目录 | `filepath.Join(configHome, "sproxy", ...)` | `"prism"` |
| 缓存目录常量 | `const cacheDirName = "sproxy"` | `"prism"` |
| 日志/跟踪前缀 | `tp.Tracer("sproxy")` | `"prism"` |
| 帮助文案 | 各 cobra `Long:` 中的 `sproxy 服务端` 等 | `prism 服务端` |

实现：内置默认映射表 + 用户可追加；替换按「最长串优先」避免子串误伤
（如先替换 `sproxy` 再处理 `sproxy 服务端` 组合，或按词边界）。

### 5.2 移除版本与命令声明

- **版本子命令**：cobra 的 `version` 子命令声明（`cmd/sclient/version.go`、
  `cmd/sproxy/version.go`）整段删除（保留文件骨架）；`--version` flag 若挂在 root 上同样摘除。
- **版本变量**：`main.Version = "dev"` / `main.BuildAt = "unknown"` 保留变量定义
  （链接器 -X 引用需要），但删除一切打印路径；或改为打印 `prism <无版本信息>`。
- **buildinfo 注入**：Makefile `GO_LD_FLAGS_X` 中的
  `-X github.com/cocomhub/buildinfo.*` 全部剔除；`internal/buildmeta` 的
  dirty_info embed 改为空。
- **遥测/追踪**：`tp.Tracer("sproxy")` 等名称随品牌表替换；不要删功能。

### 5.3 改写产物

改写后的树写入 `out/obfuscated-src/`，附带一份 `rewrite-report.txt`
（逐条列出：文件、原文、替换后文），供审计「还有没有漏网品牌串」。

### 5.4 漏网检查

改写完成后跑一个静态扫描：`grep -rn "sproxy" out/obfuscated-src/`，
预期只命中（白名单）：

1. `go.mod` / `go.sum` 中的 module path（`github.com/cocomhub/sproxy`）——
   保留理由见 §7；
2. `internal/buildmeta` 的生成文件路径注释（可清理）；
3. 改写报告文件本身（在报告内而非源码树）。

白名单之外的命中 → 改写失败（fail-closed），提示补映射。

## 6. 构建层（build）

### 6.1 构建流程

1. 在改写后的源码树内（`GOWORK=off` 逐子模块或 `go.work`）执行
   `garble -literals -tiny -seed=<seed> build`，对 `cmd/sproxy` 与 `cmd/sclient`
   两个 main 包分别产出 `prismd` 与 `bridge`。
2. `-ldflags` 只保留 `-X main.Version=hidden -X main.BuildAt=hidden`
   （或直接不注入，让默认值生效）；`-trimpath` 保留。
3. garble 依赖 `GOGARBLE`：默认混淆所有包。若个别包（如 `pkg/signature`、
   `pkg/security`）因反射/编译期行为与 garble 冲突，用 `GOGARBLE` 排除并在报告记录。
4. 产物输出到 `out/bin/`。

### 6.2 seed 策略

- `seed: random`（默认）：每次构建产出不同混淆，防批量识别。
- `seed: fixed:<hex>`：固定 seed，可复现；用于需要「所有节点二进制一致」的发布场景。
- 两者都支持，由配置开关。

### 6.3 冒烟验证

构建完成后自动执行最小冒烟：

- `prismd --help`（或等价入口）不出现 `sproxy`/`sclient`/版本号；
- `strings <binary> | grep -i sproxy` 为空（或只命中白名单）；
- `bridge`（若有）不出现子命令列表，只执行固定操作；
- 启动 `prismd`（临时端口/配置）→ 健康检查 → 退出。

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

## 8. 后续演进（非本规格范围）

- **bridge 多操作类型**：首版仅单操作（download）；后续扩展
  upload/list/delete 等，由配置固定其一。
- **密钥档位**：builtin 内嵌（混淆级保护）→ 可选 external 注入。
- **自动化**：CI 定期拉取主仓新 tag 构建并回归冒烟。
- **命名模板化**：把「显示名/命令名/路径/日志前缀」统一抽象为命名模板，
  让新代号只改一处配置。

## 9. 验收标准

1. `stealth-build obfuscate` 产出改写树，`rewrite-report.txt` 列出全部替换；
2. `stealth-build build` 产出 `prismd`/`bridge` 二进制；
3. 冒烟验证通过：`strings` 无 `sproxy`（白名单除外）、无版本号、
   bridge 无子命令、帮助无品牌；
4. 改写层漏网检查 fail-closed：白名单外命中即失败；
5. 主仓零改动（本规格不要求主仓任何 commit）；
6. 隐藏二进制可独立运行（配置内置，无需外部配置文件）。

## 10. 落地顺序建议

1. 建隐藏仓库骨架 + `stealth-build` 工具骨架（配置解析 + 拉取源码）；
2. 品牌替换表 V1（§5.1 清单）+ 漏网检查；
3. 移除版本/命令声明（§5.2）+ buildinfo 剔除；
4. garble 构建封装（seed 可配）+ 冒烟验证；
5. bridge 单操作固定形态（配置加密内嵌 + 固定执行）；
6. 文档（README/用法）与 CI。
