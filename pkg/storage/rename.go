// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"fmt"
	"time"
)

// AtomicRename 在 root 内把 srcRel 原子重命名为 dstRel（**替换**语义：目标已存在则被覆盖）。
//
// 快速路径：直接 Rename。慢速路径（Windows 并发场景）：只**重试 Rename**，以 2ms 起步的
// 短退避最多重试 5 次——Windows 上文件句柄释放有延迟，重命名会在 "Access is denied" 与
// 成功之间抖动。
//
// **绝不删除目标**：历史上慢速路径的第一步是 `_ = rt.Remove(dstRel)`（先删目标再重命名），
// 那会在快路径因**任何**原因失败时无条件销毁目标（源不存在 / 权限拒绝 / 句柄占用），重试再
// 失败即「新旧两不存」⇒ 静默数据丢失（Windows 实测：源不存在时目标被物理删除）。而删目标对
// 抖动本无帮助——目标被占用时 Remove 与 Rename 同样失败；目标可删时 rename 本身就能成功。
// 守卫：`rename_destructive_test.go` 的 TestAtomicRename_MissingSourceKeepsDestination。
//
// 路径校验由 Root 相对语义（os.Root）保证。
//
// **单一事实源**：此前 `pkg/files`（service.go）与 `pkg/server`（upload_handler.go）各有
// 一份**逐字相同**的私有实现 `atomicRenameRoot`。它是 `*storage.Root` 的存储原语（不是
// 任何领域的业务规则），故收敛到本方法；两侧保留同名薄包装（调用点零改动），由
// `pkg/server/helper_impl_drift_test.go` 的委托守卫钉住「不得重新内联重试循环」。
func (rt *Root) AtomicRename(srcRel, dstRel string) error {
	// 快速路径：直接重命名（POSIX 与 Windows 的 os.Rename 均为替换语义）。
	if err := rt.Rename(srcRel, dstRel); err == nil {
		return nil
	}
	// 慢速路径：只重试 rename（见上方「绝不删除目标」）。
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	var lastErr error
	for i := range maxAttempts {
		if lastErr = rt.Rename(srcRel, dstRel); lastErr == nil {
			return nil
		}
		if i < maxAttempts-1 {
			time.Sleep(baseDelay << i)
		}
	}
	return fmt.Errorf("重命名失败（已达最大重试次数 %d）: %w", maxAttempts, lastErr)
}
