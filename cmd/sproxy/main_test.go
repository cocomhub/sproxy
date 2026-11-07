// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestMain 在所有测试结束后清理临时目录（Windows 上测试内 RemoveAll 常因句柄未释放失败）。
func TestMain(m *testing.M) {
	code := m.Run()
	testutil.CleanupAllTempDirs()
	os.Exit(code)
}
