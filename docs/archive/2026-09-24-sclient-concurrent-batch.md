<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 并发批量（roadmap 11.5-⑨）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-concurrent-batch.md` 已于 2026-11-13 归档。
> CLI 用法见 `docs/cli.md`（现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
批量命令按文件逐行**串行**执行，大清单耗时线性。目标：`--workers N` 并发（默认 4）
+ 可选的 TTY 进度条，且**输出与串行逐字节一致**（脚本兼容）。

## 定案与取舍（仍有效）
1. **并发改变执行时序 ⇒ 靠「结果按输入行序输出」兜住兼容性**：每个结果先落到按 `Index` 的槽位，
   全部完成后按行序打印。这是引入并发后唯一被允许的可观测变化面 —— 顺序以外的输出格式不得变。
2. **`--workers 1` 是可回退的严格串行**（逐字节等价逐行执行）：并发能力必须自带「退回旧行为」的开关，
   否则脚本/CI 一旦遇到时序问题无法自救。
3. **stdout 只放终态结果，进度类信息只写 stderr**：机器解析通道与人读通道分离（脚本/管道零回归）。
4. **失败隔离**：单个 op 失败只记入结果，不中断其余；退出码沿用「任一 FAIL ⇒ 非 0」。
   批量的价值就是「跑完并报告」，中途 abort 会让部分失败的诊断成本翻倍。
5. **并发峰值由信号量（buffered chan）约束**；`workers > len(ops)` 钳位到行数（避免空 worker 空转）。
6. **batch 解析（引号/注释/子命令集）完全不动**：并发化不应夹带语法扩展。

## 未落地部分（勿误认为已交付）
设计中的 `--progress` 进度条**未接线**：`runBatchConcurrent` 的 `progress` 形参在 CLI 侧传 `nil`，
也没有注册 `--progress` flag（`ProgressReporter` / `noopProgressSink` 接口已存在但无真实消费者）。
若要补：直接在 CLI 侧注入 TTY 检测后的 sink 即可，无需改动保序/并发骨架。

## 依赖与外部契约
- CLI 契约：`--workers`（默认 4；0/1 串行退化；超行数钳位）—— 见 `docs/cli.md` 的 `batch` 节。
- batch 文件语法契约：每行一个完整子命令调用、空行与 `#` 注释跳过；当前支持的子命令子集是
  **白名单**，其余显式 FAIL（不要静默跳过未知命令）。

## 落地指针
`cmd/sclient/batch_run.go`（`runBatchConcurrent` / `Progress` / `ProgressReporter`）、
`cmd/sclient/batch_cmd.go`（flag 与接线）。
