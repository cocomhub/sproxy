// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

import (
	"os"
	"testing"
)

// TestMain 清理 e2e 二进制临时目录。
//
// e2eBinDir 由包级 sync.Once 创建（整包只构建一次二进制），生命周期跨越所有测试，
// 无法用单个测试的 t.Cleanup 清理（会删掉其它测试仍在用的二进制）。此前无人清理
// ⇒ 每次跑 e2e 都在 %TEMP% 留一个 sproxy-e2e-bin* 目录。
func TestMain(m *testing.M) {
	code := m.Run()
	if e2eBinDir != "" {
		_ = os.RemoveAll(e2eBinDir)
	}
	os.Exit(code)
}
