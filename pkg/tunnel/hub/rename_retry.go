// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"time"
)

// rename_retry.go 提供**有界重试的原子替换**（temp + rename 落盘的收尾一步）。
//
// 动机（flake 收口 2026-10-14，Windows 实测）：`os.Rename` 覆盖一个**正被其它句柄占用**的目标
// （同进程读侧刚 Stat/Load、杀软/索引器扫描、CI 上并发测试进程）在 Windows 上会失败并报
// `... being used by another process`（sharing violation），而在 POSIX 上恒成功 ⇒ 表现为
// 「偶发落盘失败」（hub 联邦候选 `federation-candidates.json` / 路由表快照）。
// 这不是逻辑错误而是**平台语义差异**，有界退避重试是标准处置（给占用者释放句柄的时间）。
//
// **不弱化正确性**：重试耗尽仍返回错误（调用方照旧记录日志/上抛）——重试只把「瞬时竞争」与
// 「真实故障」区分开，绝不静默吞掉落盘失败。
const (
	renameRetryAttempts = 5
	renameRetryBackoff  = 20 * time.Millisecond
)

// renameWithRetry 调用 rename(tmp, dst)，失败时做有界退避重试；全部失败返回最后一次错误
// （附尝试次数便于归因）。rename 以参数注入：单测可注入「前 N 次失败」以确定性验证重试路径
// （Windows sharing violation 无法在 POSIX 上复现，也不宜用全局可变态注入）。
func renameWithRetry(rename func(string, string) error, tmp, dst string) error {
	var err error
	for i := 0; i < renameRetryAttempts; i++ {
		if err = rename(tmp, dst); err == nil {
			return nil
		}
		if i < renameRetryAttempts-1 {
			time.Sleep(renameRetryBackoff)
		}
	}
	return fmt.Errorf("hub: 原子替换 %s 失败（%d 次尝试后）: %w", dst, renameRetryAttempts, err)
}
