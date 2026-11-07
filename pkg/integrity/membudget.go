// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

// MemEstimator 是可选接口：Checker 实现后可预估一次 Check 所需的内存占用（字节）。
// 供调度层做内存配额治理（ByteSize 配置 + 并发排队等待释放；单文件估算超配额则
// 跳过校验标记 unverified）。0 = 未知/不占用额外内存（调度层按无内存需求处理）。
type MemEstimator interface {
	// EstimateMem 返回一次 Check 的峰值内存占用估算（bytes）。path/size 与 Check 同参。
	// 实现应保守（高估优于低估——低估可能 OOM）。
	EstimateMem(path string, size int64) int64
}

// MemEstimateOf 提取 Checker 的估算（未实现 MemEstimator 返回 0）。
func MemEstimateOf(c Checker, path string, size int64) int64 {
	if me, ok := c.(MemEstimator); ok {
		return me.EstimateMem(path, size)
	}
	return 0
}
