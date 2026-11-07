// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "testing"

// TestLogicalKey_IsZeroAndString 钉住 `LogicalKey` 的**零值语义**与可读形式。
//
// 零值语义是**契约**（设计 §9.5）：`LogicalKey{}` = **未声明归属** ⇒ 消费方（配额层）
// 必须**显式不记账**，**禁**按路径反推（封装卷路径不可归属）。
func TestLogicalKey_IsZeroAndString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  LogicalKey
		zero bool
		want string
	}{
		{"零值", LogicalKey{}, true, "<no-attribution>"},
		{"仅 owner", LogicalKey{Owner: "alice"}, false, "alice"},
		{"完整", LogicalKey{Owner: "alice", Rel: "user/a.bin"}, false, "alice/user/a.bin"},
		{"仅 rel（仍非零，消费方按 owner 空处置）", LogicalKey{Rel: "user/a.bin"}, false, "/user/a.bin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.key.IsZero(); got != c.zero {
				t.Fatalf("IsZero()=%v，want %v", got, c.zero)
			}
			if got := c.key.String(); got != c.want {
				t.Fatalf("String()=%q，want %q", got, c.want)
			}
		})
	}
}
