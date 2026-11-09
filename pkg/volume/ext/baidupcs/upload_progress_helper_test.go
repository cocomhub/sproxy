// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeTemp 写一个指定字节数的临时文件（供 uploadStaged 夹具使用）。
func writeTemp(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "staged.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}
