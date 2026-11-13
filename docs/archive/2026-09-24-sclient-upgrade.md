<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 自更新（`sclient upgrade`）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-upgrade.md` 已于 2026-11-13 归档。
> 命令用法以 `docs/cli.md`（§upgrade）为准（现行事实源），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
发布产物由 GoReleaser 产出（含 `checksums.txt`）。目标是让 `sclient` 能从 GitHub Releases
自我升级，且**替换前必须校验**（不能拿到半个二进制就把自己换掉）。

## 定案与取舍（仍有效）
1. **核心逻辑独立成包 `pkg/selfupdate`**：纯函数 + 可注入 `http.Client` ⇒ 全部可离网测试
   （下载/解包/替换都不依赖真实 GitHub）。HTTP 必须用 `netutil.IsolatedTransport()`（R19）。
2. **checksum 是唯一信任锚，fail-closed**：`checksums.txt` 缺失/无该制品/摘要不符 ⇒ 删除临时
   文件并失败。release 期间 `latest` tag 漂移也不怕——校验兜底。
3. **解包只认一个条目名**（`sclient` / `sclient.exe`），并拒绝 `..`、绝对路径、符号链接、
   超尺寸条目（防 zip-slip）。不是「解出所有条目再挑」。
4. **替换两段式兜底**：`chmod 0755` + `os.Rename`；Windows 允许重命名运行中的 exe ⇒
   先 `target→target.old` 再换入，仍失败写 `target.upgrade.bat`（提示用户退出后运行完成替换）。
   **绝不**在替换失败时留下「既没新也没旧」的状态。
5. **并发防重入用 `target.upgrade.lock`**（`O_CREATE|O_EXCL`），完成后删除。
6. **不改 `OutputFormatter`**：本地 result 结构 + Text/JSON 打印（读既有 `--json`）。
7. **版本无法判定就明说**：当前版本含 `SNAPSHOT`/`dirty` 后缀时对比失败 ⇒ 提示「无法判定」，
   `--force` 才强升；**不为对比失败 panic**。`--check` 恒 exit 0（脚本解析 `--json`
   的 `update_available`）。
8. 每次调用只打一次 API（`releases/latest` 或 `releases/tags/{tag}`），下载走制品直链，
   避开未认证限流。

## 落地指针
`pkg/selfupdate/selfupdate.go`（`Latest` / `ByTag` / `AssetName` / `NormalizeVersion` /
`CompareVersions` / `FindAsset` / `Checksums` / `DownloadAndVerify` / `ExtractBinary` /
`SwapBinary`）、`cmd/sclient/upgrade.go`（`--check` / `--to` / `--force`，隐藏 `--api-base`
供测试注入）。
