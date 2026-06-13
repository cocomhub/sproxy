// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"testing"
)

// FuzzParseKey 检查 ParseKey 在各种 hex 输入下不 panic。
func FuzzParseKey(f *testing.F) {
	seeds := []string{
		"",
		"a",
		"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"not-hex-string-!!",
		"abcdefghijklmnopqrstuvwxyz123456",
		"010203",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, hexKey string) {
		key, err := ParseKey(hexKey)
		if err != nil {
			// 错误可接受，但不能 panic
			if key != nil {
				t.Errorf("ParseKey(%q) returned non-nil key on error: %v", hexKey, key)
			}
			return
		}
		// 成功的解析：key 必须为 32 字节
		if len(key) != 32 {
			t.Errorf("ParseKey(%q) returned key of len %d, want 32", hexKey, len(key))
		}
	})
}
