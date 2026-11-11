// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// issues_executors.go 把领域侧的**修复能力**组装成 `issues.RetryFunc`（待处理事项的自动重试执行器）。
//
// 用户裁定 2026-11-10：「记录失败事件审计重试能力，后续定期重试处理，**最终必须成功**」。
// 机制侧（`pkg/server/issues`）已具备「指数退避 + 有界 + 达上限转人工 + 无执行器转人工」；
// 本文件补齐**执行器**，使"重试"真正有动作可做，闭环为：
//
//	发现问题（启动扫描 / 改名失败上报） → 登记事项 → 定期重试 → 成功解除 / 有界后转人工
//
// 设计约定：
//   - 依赖经 `ExecutorDeps` **显式注入**（不读全局），便于测试与装配顺序解耦；
//   - 领域包能力用**最小接口**（`DirMetaCleaner`）表达，`pkg/server` 不必依赖具体卷实现类型；
//     取卷一律走 `Handlers.VolumeViewFS`（统一卷视图入口），不在本文件直接接触后端视图；
//   - 执行器**必须幂等**（重试可能重复执行）：领域能力本身已保证（0 个残留也算成功）。

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// DirMetaCleaner 是「清理非权威目录 meta」的最小能力接口
// （`pkg/volume/secretdata` 的 `SecretdataFS` 已满足；用最小接口保持解耦）。
//
// 约定：**幂等** —— 无残留（0/1 个目录 meta）时返回 `(0, nil)`，因此可安全重复执行。
type DirMetaCleaner interface {
	CleanupStaleDirMeta(ctx context.Context, container string) (int, error)
}

// ExecutorDeps 是执行器所需的最小依赖（显式注入，便于测试与解耦装配顺序）。
type ExecutorDeps struct {
	// Resolve 返回指定卷的「可修复对象」（生产经 `Handlers.VolumeViewFS`，装配期已套装饰链与封印）。
	// 返回 nil 表示该卷不可用/不存在 ⇒ 按"暂时失败"处理（退避重试，达上限转人工）。
	Resolve func(volume string) any
	// Log 可选（nil ⇒ 不记日志）。
	Log *slog.Logger
}

func (d ExecutorDeps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// NewIssuesExecutors 组装「事项类型 → 重试执行器」映射，交给 `IssueCenter.RunIssuesOnce` 使用。
//
// 当前覆盖：
//   - `kind=dir_meta_cleanup_failed`：调用卷的 `CleanupStaleDirMeta(container)`。
//     事项的 `Path` 存的就是**容器路径**（上报时即按执行器需要的入参存放；逻辑路径在 `Detail` 里）。
//
// 未覆盖的类型**不注册**（`RunDue` 会把它们转 `needs_human` 并写明原因）——
// 这比"注册一个什么都不做的执行器"诚实：后者会让人误以为在自动重试。
func NewIssuesExecutors(d ExecutorDeps) map[issues.Kind]issues.RetryFunc {
	return map[issues.Kind]issues.RetryFunc{
		issues.KindDirMetaCleanupFailed: func(ctx context.Context, it issues.Item) error {
			return runDirMetaCleanup(ctx, d, it)
		},
	}
}

// runDirMetaCleanup 执行一次「清理非权威目录 meta」重试。
func runDirMetaCleanup(ctx context.Context, d ExecutorDeps, it issues.Item) error {
	if it.Volume == "" {
		return fmt.Errorf("事项缺少卷名，无法定位修复目标（需人工处理）")
	}
	if it.Path == "" {
		return fmt.Errorf("事项缺少容器路径，无法定位修复目标（需人工处理）")
	}
	if d.Resolve == nil {
		return fmt.Errorf("未注入卷解析器（Resolve 为空），无法执行修复")
	}
	target := d.Resolve(it.Volume)
	if target == nil {
		return fmt.Errorf("卷 %q 当前不可用（可能未装配/已移除）", it.Volume)
	}
	cleaner, ok := target.(DirMetaCleaner)
	if !ok {
		return fmt.Errorf("卷 %q 不支持目录 meta 清理（该类型无此能力），需人工介入", it.Volume)
	}
	removed, err := cleaner.CleanupStaleDirMeta(ctx, it.Path)
	if err != nil {
		return err
	}
	// 幂等成功：removed 可能为 0（前一次已清干净，或本就无残留）——两者都算"已解决"。
	d.log().Info("待处理事项重试成功：已清理非权威目录 meta",
		"volume", it.Volume, "container", it.Path, "removed", removed, "kind", string(it.Kind))
	return nil
}
