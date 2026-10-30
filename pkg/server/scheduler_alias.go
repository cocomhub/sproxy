// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// scheduler_alias.go 是**迁移期兼容层**：统一周期任务调度器已下沉 pkg/server/scheduler，
// 装配层用类型别名保持既有字段/字面量零改动。
//
// **移除条件**：装配层把周期任务注册整体搬入各切片后删除本文件；新代码直接使用
// pkg/server/scheduler。
package server

import "github.com/cocomhub/sproxy/pkg/server/scheduler"

type (
	// Scheduler 是统一周期任务调度器。
	Scheduler = scheduler.Scheduler
	// Task 是注册到调度器的周期任务描述。
	Task = scheduler.Task
	// TaskFunc 是任务执行体。
	TaskFunc = scheduler.TaskFunc
)
