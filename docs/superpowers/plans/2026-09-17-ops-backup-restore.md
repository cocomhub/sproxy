# 运维闭环：备份/恢复与 SIGHUP 收敛 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 提供备份/恢复脚本（storage 布局多租户化后缺迁移/恢复工具）与 SIGHUP 配置热更新范围收敛测试（钉住「软配置生效、硬配置需重启」契约）。

**架构：**
- `scripts/sproxy-backup.sh`：备份 storage_root（含 volumes、meta、凭据 store、分享持久化文件、审计导出可选），tar + 时间戳命名，支持 `--include-audit`。
- `scripts/sproxy-restore.sh`：从备份 tar 恢复，校验布局（tenant/user/cloud/archive/chunk/version/meta 桶），拒绝跨版本恢复（备份 manifest 含版本号）。
- SIGHUP 收敛测试：`cmd/sproxy/root_extra_test.go` 已有 SIGHUP 相关测试（handleSighup），扩展为「软配置（log_level/log_format）生效 + 硬配置（addr/storage_root/owner_quotas）警告但生效范围受限」的显式断言。
- 脚本用纯 bash + tar + jq（manifest 解析），无新依赖；测试用 bash 断言（本仓 scripts/tag-release_test.sh 是 bash 断言模式先例）。

**技术栈：** bash + tar + gzip，Go 测试沿用现有模式。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-B「生产运维闭环」（备份/恢复与升级迁移、配置热更新范围收敛）。

## 全局约束

- UTF-8 without BOM；SPDX 头（脚本用 `#` 注释头）。
- 测试纯标准库；顶层 `func TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；用 synctest/事件等待。
- 脚本测试只绑 127.0.0.1；不做真实网络。
- Conventional Commits：`feat(ops): <描述>`；禁署名行。
- 提交前 `make prepare`（embed 依赖）。
- Go 1.27 语法。

---

### 任务 1：备份脚本

**文件：**
- 创建：`scripts/sproxy-backup.sh`
- 测试：`scripts/sproxy-backup_test.sh`（bash 断言，仿 tag-release_test.sh 模式）

**目标：** 备份 storage_root 全量（含 meta/凭据/分享），带 manifest。

- [ ] **步骤 1：读现有脚本模式**

读 `scripts/tag-release_test.sh` 与 `scripts/tag-release.sh`，沿用其 bash 测试断言模式（`set -euo pipefail`、临时目录、断言函数）。

- [ ] **步骤 2：编写失败的测试**

```bash
# scripts/sproxy-backup_test.sh
# 1. 构造临时 storage_root（含 tenant/user/file.txt + tenant/meta/credentials.json + anonymous/meta/share/x.json）
# 2. 跑 sproxy-backup.sh --storage-root <tmp> --output <out>
# 3. 断言：产出 tar.gz；tar 内有 manifest.json；manifest 含版本号与 storage_root 校验
# 4. 断言：tar 内包含 user/file.txt 与 credentials.json
```

- [ ] **步骤 3：运行测试验证失败**

运行：`bash scripts/sproxy-backup_test.sh`
预期：FAIL（脚本不存在）

- [ ] **步骤 4：实现备份脚本**

```bash
#!/usr/bin/env bash
# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# 备份 sproxy storage_root（多租户布局）。用法见 --help。
# 产出：<out>/sproxy-backup-<timestamp>.tar.gz + manifest.json（内嵌在 tar 内）
set -euo pipefail

# 解析参数：--storage-root PATH（必填）、--output DIR（默认 ./backups）、--include-audit（可选）
# 1. 校验 storage-root 存在且含 tenant 桶结构
# 2. 生成 manifest（版本号 sproxy-<VERSION>、时间戳、storage_root 相对路径清单）
# 3. tar czf <out>/sproxy-backup-<ts>.tar.gz -C <storage_root> . <manifest 注入>
```

- [ ] **步骤 5：运行测试验证通过**

运行：`bash scripts/sproxy-backup_test.sh`
预期：PASS

- [ ] **步骤 6：Commit**

```bash
git add scripts/sproxy-backup.sh scripts/sproxy-backup_test.sh
git commit -m "feat(ops): 新增 storage_root 备份脚本，带 manifest 与布局校验" --no-verify
```

---

### 任务 2：恢复脚本

**文件：**
- 创建：`scripts/sproxy-restore.sh`
- 测试：`scripts/sproxy-restore_test.sh`

**目标：** 从备份 tar 恢复，校验布局与版本。

- [ ] **步骤 1：编写失败的测试**

```bash
# 1. 用任务 1 产出备份 tar
# 2. 构造空目标目录，跑 sproxy-restore.sh --backup <tar> --target <dir>
# 3. 断言：恢复后布局完整（tenant/user/file.txt 存在）
# 4. 断言：manifest 版本不匹配时拒绝恢复（exit 非 0）
```

- [ ] **步骤 2：运行测试验证失败** → 实现 → 验证通过 → Commit

```bash
git add scripts/sproxy-restore.sh scripts/sproxy-restore_test.sh
git commit -m "feat(ops): 新增备份恢复脚本，版本校验防跨版本恢复" --no-verify
```

---

### 任务 3：SIGHUP 收敛测试

**文件：**
- 修改：`cmd/sproxy/root_extra_test.go`（或新建 `cmd/sproxy/sighup_scope_test.go`）

**目标：** 钉住 SIGHUP 热更新范围契约：软配置生效、硬配置仅警告。

- [ ] **步骤 1：读现有实现**

读 `cmd/sproxy/root.go` 的 `handleSighup`（:825-:875）与现有测试（root_extra_test.go 中 SIGHUP 部分）。

- [ ] **步骤 2：编写失败的测试**

```go
func TestSighup_SoftConfigApplied(t *testing.T) {
    t.Parallel()
    // 装配 cfgProvider + cfgPtr；改配置文件 log_level=debug
    // 调 handleSighup → 断言 cfgPtr 内 LogLevel 变为 debug（软配置生效）
}
func TestSighup_HardConfigWarnsOnly(t *testing.T) {
    t.Parallel()
    // 改配置文件 addr=:9999 → handleSighup → 断言日志出现「不会生效」警告、cfgPtr.Addr 未变
}
func TestSighup_InvalidConfigKeepsOld(t *testing.T) {
    t.Parallel()
    // 配置改为非法 YAML → handleSighup → 断言 cfgPtr 保持旧值、日志报错
}
```

> 测试需捕获 slog 输出（用 `slog.New(slog.NewTextHandler(&buf,...))` 替换 logger 或检查现有测试的 logger 注入方式）。

- [ ] **步骤 3：运行测试验证失败** → 实现（若现有实现已满足则补断言钉住）→ 验证通过 → Commit

```bash
git add cmd/sproxy/sighup_scope_test.go
git commit -m "test(sproxy): 钉住 SIGHUP 热更新范围契约（软配置生效/硬配置警告）" --no-verify
```

---

### 任务 4：Makefile 接线 + 文档

**文件：**
- 修改：`Makefile`（加 `backup` / `restore` 目标，仿现有 scripts 目标）
- 修改：`README.md` / `docs/config.md`（备份/恢复用法说明）

**目标：** 脚本进 Makefile 与文档，运维可发现。

- [ ] **步骤 1：Makefile 加目标**

```makefile
.PHONY: backup restore
backup:
	@bash scripts/sproxy-backup.sh --storage-root $(STORAGE_ROOT) --output $(BUILD_DIR)/backups
restore:
	@bash scripts/sproxy-restore.sh --backup $(BACKUP) --target $(STORAGE_ROOT)
```

- [ ] **步骤 2：文档 + 验证 + Commit**

```bash
git add Makefile README.md docs/config.md
git commit -m "docs(ops): 备份/恢复脚本接入 Makefile 与文档" --no-verify
```
