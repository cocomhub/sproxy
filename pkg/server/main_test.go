// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestMain 在所有测试结束后统一清理**显式 TempDir** 类残留。
//
// 背景（2026-10-31 实测）：%TEMP% 累积了约 1,800 个本仓测试目录。其中
// `nested-secret-tmp*` 648 个来自测试封装处理层传给 secretdata 的显式 TempDir——
// secretdata 的契约是「显式配置的 TempDir 不清理（调用方所有）」，而 Build 没有
// *testing.T，无法用 t.Cleanup，故改为进程级统一清理（不能放在单个测试里：并行
// 测试可能仍在用）。
func TestMain(m *testing.M) {
	code := m.Run()
	cleanupAllNestedTempDirs()
	// 兜底：Windows 上测试内 RemoveAll 常因句柄未释放失败，全部测试结束后统一清。
	testutil.CleanupAllTempDirs()
	os.Exit(code)
}
