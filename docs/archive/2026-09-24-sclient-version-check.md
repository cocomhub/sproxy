<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 版本自检 — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-version-check.md` 已于 2026-11-14 归档。
> 现行能力见 `docs/cli.md` §upgrade。本文件只保留**设计意图 / 取舍 / 仍有效的判断**，并记录
> **设计 ⇄ 实现的分叉**（能力已落地，但形态与本文不同）。

## 背景 / 目标
`sclient version` 只组装 buildinfo 并输出，无网络对比能力。目标：低频提示「有新版本」
（对比本地版本 vs GitHub latest release）——**不阻塞、不自动更新、不改变主命令退出码**。

## 设计 ⇄ 实现分叉（务必阅读）
- 设计设想为 `sclient version --check`（新文件 `version_check.go` 或 `pkg/versioncheck` 包），
  组件 `ParseLatest` / `CompareVersion` / `CheckCached`（XDG 缓存 + TTL 低频）。
- **实际落地形态 = `sclient upgrade --check`**：由 `pkg/selfupdate`（`Client.Latest` / `CompareVersions`）
  承担版本对比，`cmd/sclient/upgrade.go` 的 `upgradeDecide` / `printUpgradeCheck` 输出提示；
  **`version --check` 与 XDG TTL 缓存均未实现**（`upgrade --check` 每次查询 GitHub；
  未认证限流 60 次/时由使用者频率控制，未做本地缓存）。`version` 命令最终只是 buildinfo + `dirty-info`。
- 因此归档时以 `docs/cli.md` §upgrade 为权威指针，本文的具体组件名/缓存设计**已作废**。

## 仍有效的判断（已随实现保留）
1. **版本自检是低频附加能力，不得影响主命令**：`upgrade --check` 恒 **exit 0**，网络/解析失败静默降级
   （脚本解析 `--json` 的 `update_available` 字段）。
2. **版本字符串不可解析时（SNAPSHOT/dirty）不误报**：`--check` 提示「无法判定」（视为最新）；
   升级需 `--force`。
3. **网络使用规范传输**（`pkg/netutil.IsolatedTransport()`，禁裸 `http.Transport`，R19）+ 超时；
   UA 标识客户端、`Accept: application/vnd.github+json`。
4. **隐私**：仅请求 `releases/latest` 公开端点，无用户数据外发。

## 落地指针
- `cmd/sclient/upgrade.go`：`NewCmdUpgrade` / `newUpgradeCmd` / `upgradeDecide` / `printUpgradeCheck`；
- `pkg/selfupdate/selfupdate.go`：`New` / `Latest` / `ByTag` / `NormalizeVersion` / `CompareVersions` / `FindAsset`；
- 测试：`cmd/sclient/upgrade_test.go`（`--check` 全链路 + 恒 exit 0）、`pkg/selfupdate/selfupdate_test.go`。
