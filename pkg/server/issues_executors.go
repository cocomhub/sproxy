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
//   - 领域包能力用**能力接口**（`syncpkg.DirMetaCleaner`）表达，`pkg/server` 不必依赖具体卷
//     实现类型；取卷统一走 `CleanupTargetResolver`（`VolumeView` + **直接断言装饰后的 FS**，
//     与 `ScanTargetsFromSet` 同一收口做法）——不再经 `External(vol)` 的后端 FS 视图直连（R27），
//     也不再 `Innermost` 解包（能力已由 R21 保证在 11 个装饰器上全量转发）。
//   - 执行器**必须幂等**（重试可能重复执行）：领域能力本身已保证（0 个残留也算成功）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// CleanupTargetResolver 返回「事项修复目标解析器」：按卷名取**统一卷视图**（`VolumeView`）
// 并**直接返回装饰后的 FS**（不再穿透装饰链，与 `ScanTargetsFromSet` 的收口做法一致）。
//
// 为什么直接断言装饰后的 FS 即可（2026-11-12 收口；根治见
// docs/decorator-capability-forwarding.md）：`syncpkg.DirMetaCleaner` 已下沉 `pkg/sync`
// 且 R21 门禁要求 11 个装饰器全部转发它 ⇒ 装饰链不再吞掉该能力，`Innermost` 解包随之消除
// （能力留在消费包时装饰器无法引用 ⇒ 断言恒 false ⇒ 只能解包；下沉后无需解包）。
// 能力断言仍按「可选能力」口径处置：底层未实现时转发层返回 `ErrUnsupported`，
// 由调用方按"未实现"处理（而不是让断言失败静默跳过）。
//
// 返回 nil ⇒ 卷不可用/不存在（调用方按“暂时失败”重试，达上限转人工）。
func CleanupTargetResolver(l VolumeLister) func(volume string) any {
	return func(volume string) any {
		if l == nil {
			return nil
		}
		fsys, ok := l.VolumeView(volume, registry.ViewUser)
		if !ok || fsys == nil {
			return nil
		}
		return fsys
	}
}

// ExecutorDeps 是执行器所需的最小依赖（显式注入，便于测试与解耦装配顺序）。
type ExecutorDeps struct {
	// Resolve 返回指定卷的「可修复对象」（生产由 `cmd/sproxy` 装配注入，取后端视图并交给领域能力）。
	// 返回 nil 表示该卷不可用/不存在 ⇒ 按"暂时失败"处理（退避重试，达上限转人工）。
	Resolve func(volume string) any
	// Log 可选（nil ⇒ 不记日志）。
	Log *slog.Logger
	// ReconcileBlobMD5 执行一次「内容一致但 md5 未权威」的重试：**连续上传 2 次**（第 2 次几近必为
	// 秒传命中 ⇒ 触发 md5 刷新）并**重新逐块对账**。
	//
	// 参数：volume=卷名，rel=卷内相对路径，localCopy=上传时**保留的本地副本路径**（重传源）。
	// 返回 nil ⇒ 已收敛（`RunDue` 会 markResolved）；返回错误 ⇒ 退避重试，达上限转人工。
	//
	// ⚠️ **前置**：该项生效要求上传路径在「`block_list` 一致但 md5 未权威」时**保留本地副本**
	// （否则本动作无源可传，只会退避到 needs_human）。未注入（nil）⇒ 该类型不注册（转人工）。
	ReconcileBlobMD5 func(ctx context.Context, volume, rel, localCopy string) error
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
		issues.KindBlobMD5Unconverged: func(ctx context.Context, it issues.Item) error {
			return runBlobMD5Reconcile(ctx, d, it)
		},
	}
}

// runBlobMD5Reconcile 执行一次「blob md5 未收敛」重试（连续上传 2 次 + 重新对账）。
//
// 登记约定（见 `issues.KindBlobMD5Unconverged`）：`Volume`=卷名、`Path`=**本地副本路径**、
// `Detail`=卷内 rel。三者缺一即**返回错误转人工**（不静默成功，避免“看起来在重试”）。
func runBlobMD5Reconcile(ctx context.Context, d ExecutorDeps, it issues.Item) error {
	if it.Volume == "" {
		return fmt.Errorf("事项缺少卷名，无法定位卷（需人工处理）")
	}
	if it.Path == "" {
		return fmt.Errorf("事项缺少本地副本路径（上传时未保留副本？），无法重传（需人工处理）")
	}
	if it.Detail == "" {
		return fmt.Errorf("事项缺少卷内路径 rel，无法定位目标（需人工处理）")
	}
	if d.ReconcileBlobMD5 == nil {
		return fmt.Errorf("未注入 blob md5 重试执行器（ReconcileBlobMD5 为空），无法执行")
	}
	return d.ReconcileBlobMD5(ctx, it.Volume, it.Detail, it.Path)
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
	cleaner, ok := syncpkg.AssertDirMetaCleaner(target)
	if !ok {
		return fmt.Errorf("卷 %q 不支持目录 meta 清理（该类型无此能力），需人工介入", it.Volume)
	}
	removed, err := cleaner.CleanupStaleDirMeta(ctx, it.Path)
	if err != nil {
		// 转发层对「底层无此能力」返回 ErrUnsupported（装饰链无条件实现该接口，故断言不再
		// 能区分）；此时必须给出可读原因（运维按「详细原因」接入人工），而不是裸 ErrUnsupported。
		if errors.Is(err, syncpkg.ErrUnsupported) {
			return fmt.Errorf("卷 %q 的底层存储不支持目录 meta 清理（装饰链未转发/该卷类型无此能力），需人工介入", it.Volume)
		}
		return err
	}
	// 幂等成功：removed 可能为 0（前一次已清干净，或本就无残留）——两者都算"已解决"。
	d.log().Info("待处理事项重试成功：已清理非权威目录 meta",
		"volume", it.Volume, "container", it.Path, "removed", removed, "kind", string(it.Kind))
	return nil
}
