<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一周期任务调度器（`pkg/server/scheduler`）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-task-scheduler.md` 已于 2026-11-13 归档。
> 配置以 `docs/config.md`（§统一任务调度器 `scheduler`）、架构位置以 `docs/architecture.md`（§S3）
> 为准（现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
版本 GC、分享清理、usage flush、卷级 retention 等周期任务各自起 goroutine，每个都重复
「ticker + stop channel + WaitGroup + 窗口判断 + defer recover」样板。目标是把**调度**
收成一个组件，任务只描述「做什么」。

## 定案与取舍（仍有效）
1. **任务描述即 `scheduler.Task`**，调度器为零 `pkg/*` 依赖的 L0 子包（`pkg/server/scheduler`）
   ⇒ 不引入隐式依赖环，可被任何装配层复用。
2. **单飞防重入**：`CompareAndSwap(false,true)` 失败即 Warn「上次未结束，跳过本 tick」——
   **跳过而不是排队**：长任务 + 短间隔下，排队会把积压变成雪崩。
3. **`runOnce` 内 recover**（Error 日志带任务名）⇒ 单个任务 panic 不杀死循环，下个 tick 照常；
   任务自身的 recover 应删除以避免双保险掩盖问题。
4. **窗口在装配层解析**为 `inWindow func(time.Time) bool`；非法 `HH:MM` 在 Validate 期拒绝
   （fail-closed，配置错误响亮暴露）；`End <= Start` 视为**跨天窗口**。
5. **`Register` 的坏输入（重名 / Interval<=0 / Name 空）返回 error**，装配层记日志后跳过该任务，
   **不 panic**（一个任务配错不该拖垮进程启动）。
6. **`Stop` 无超时**：任务挂死会阻塞停服——与既有逐循环 `wg.Wait()` 语义一致，属**已知**，
   文档明示；要超时属独立改造。

## 落地指针
`pkg/server/scheduler/scheduler.go`（`Register` / `Start` / `Stop` / `Task`）、
`scheduler_test.go`；装配根注册各任务（upload 清理 / share 清理 / 版本 GC / usage flush /
卷级 retention 等）。
