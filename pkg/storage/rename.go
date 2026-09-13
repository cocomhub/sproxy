// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"fmt"
	"time"
)

// AtomicRename 在 root 内把 srcRel 原子重命名为 dstRel。
//
// 快速路径：直接 Rename。慢速路径（Windows 并发场景）：先删除目标再重命名，并以 2ms
// 起步的短退避最多重试 5 次——Windows 上文件句柄释放有延迟，删除/重命名会在
// "Access is denied" 与成功之间抖动。路径校验由 Root 相对语义（os.Root）保证。
//
// **单一事实源**：此前 `pkg/files`（service.go）与 `pkg/server`（upload_handler.go）各有
// 一份**逐字相同**的私有实现 `atomicRenameRoot`。它是 `*storage.Root` 的存储原语（不是
// 任何领域的业务规则），故收敛到本方法；两侧保留同名薄包装（调用点零改动），由
// `pkg/server/helper_impl_drift_test.go` 的委托守卫钉住「不得重新内联重试循环」。
func (rt *Root) AtomicRename(srcRel, dstRel string) error {
	// 快速路径：直接重命名
	if err := rt.Rename(srcRel, dstRel); err == nil {
		return nil
	}
	// 慢速路径：删除目标文件，然后重命名临时文件
	// 使用短退避重试，解决 Windows 上并发 Rename 导致的"Access is denied"
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	for i := range maxAttempts {
		_ = rt.Remove(dstRel)
		if err := rt.Rename(srcRel, dstRel); err == nil {
			return nil
		} else if i == maxAttempts-1 {
			return fmt.Errorf("重命名失败（已达最大重试次数 %d）: %w", maxAttempts, err)
		}
		time.Sleep(baseDelay << i)
	}
	return nil
}
