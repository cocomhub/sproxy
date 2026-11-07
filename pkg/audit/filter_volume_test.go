// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"testing"
	"time"
)

// TestEntryFilter_Volume 卷维度筛选（评审 R13：Entry.Volume 此前无法被筛选）。
func TestEntryFilter_Volume(t *testing.T) {
	t.Parallel()
	v1 := Entry{Scope: "system", Kind: KindEvent, Action: "file.write", Volume: "v1", TS: time.Unix(1_800_000_000, 0)}
	v2 := Entry{Scope: "system", Kind: KindEvent, Action: "file.write", Volume: "v2", TS: time.Unix(1_800_000_000, 0)}
	empty := Entry{Scope: "system", Kind: KindEvent, Action: "file.write", TS: time.Unix(1_800_000_000, 0)}

	if !(EntryFilter{Volume: "v1"}).Match(v1) {
		t.Fatal("命中卷应通过")
	}
	if (EntryFilter{Volume: "v1"}).Match(v2) {
		t.Fatal("其它卷应被过滤")
	}
	if (EntryFilter{Volume: "v1"}).Match(empty) {
		t.Fatal("空卷不得命中具体卷筛选")
	}
	if !(EntryFilter{}).Match(empty) {
		t.Fatal("未指定 Volume 表示不过滤（旧条目零回归）")
	}
}
