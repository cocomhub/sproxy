// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"strings"
	"testing"
)

// TestReconcileBlockList_逐块对账 锁定 P1 的核心判据（依据 docs/external-dependencies.md §7.8.5 实测）：
// 远端 `block_list` 是**按 4 MiB 分块的真实内容 md5 列表**，与本地 `stagedUpload.blockMD5s` 同构
// ⇒ **逐块 EqualFold 相等** 才能判定「内容已在云端」。
//
// 关键：必须区分三种结果，**不可把"取不到/非法"当成一致**：
//   - equal=true  & decidable=true  ⇒ 内容已在云端（可对客户端返回成功）
//   - equal=false & decidable=true  ⇒ 真不一致（重传）
//   - decidable=false               ⇒ **不可判定**（长度不符/空/含非法值）⇒ 必须走"保留副本 + 重试"
func TestReconcileBlockList_逐块对账(t *testing.T) {
	t.Parallel()
	a := strings.Repeat("a", 32)
	b := strings.Repeat("b", 32)
	c := strings.Repeat("c", 32)
	upper := strings.ToUpper(a)

	cases := []struct {
		name          string
		remote, local []string
		wantEqual     bool
		wantDecidable bool
	}{
		{"单块相等（大小写不敏感）", []string{upper}, []string{a}, true, true},
		{"多块逐块相等", []string{a, b, c}, []string{a, b, c}, true, true},
		{"多块顺序不符 ⇒ 真不一致", []string{b, a, c}, []string{a, b, c}, false, true},
		{"某块内容不同 ⇒ 真不一致", []string{a, b, c}, []string{a, b, "d" + strings.Repeat("0", 31)}, false, true},
		{"长度不同（远端少）⇒ 不可判定", []string{a}, []string{a, b}, false, false},
		{"长度不同（远端多）⇒ 不可判定", []string{a, b, c}, []string{a}, false, false},
		{"远端为空 ⇒ 不可判定", nil, []string{a}, false, false},
		{"本地为空 ⇒ 不可判定", []string{a}, nil, false, false},
		{"双方都空 ⇒ 不可判定", nil, nil, false, false},
		{"远端含空串 ⇒ 不可判定", []string{a, ""}, []string{a, b}, false, false},
		{"远端含非法长度 ⇒ 不可判定", []string{a, "deadbeef"}, []string{a, b}, false, false},
		{"远端含非 hex ⇒ 不可判定", []string{"z" + strings.Repeat("0", 31)}, []string{a}, false, false},
		{"本地含非法值 ⇒ 不可判定", []string{a}, []string{"short"}, false, false},
	}
	for _, tc := range cases {
		eq, dec := reconcileBlockList(tc.remote, tc.local)
		if eq != tc.wantEqual || dec != tc.wantDecidable {
			t.Errorf("%s: got (equal=%v, decidable=%v), want (equal=%v, decidable=%v)",
				tc.name, eq, dec, tc.wantEqual, tc.wantDecidable)
		}
	}
}

// TestMD5Authoritative_md5对账 锁定 P1 的第二判据：**必须值相等且形态合法**。
// 实测反例（§7.8.5）：远端 md5 曾返回 `a6804cf3…`（**合法 32hex 但不是整文件 md5**）
// ⇒ **只看形态会误判权威** ⇒ 必须与实际值 `EqualFold` 比对。
func TestMD5Authoritative_md5对账(t *testing.T) {
	t.Parallel()
	want := strings.Repeat("1", 32)
	cases := []struct {
		name         string
		remote, want string
		expect       bool
	}{
		{"完全相等", want, want, true},
		{"大小写不同但值相等", strings.ToUpper(want), want, true},
		{"形态合法但值不同 ⇒ 不权威", strings.Repeat("2", 32), want, false},
		{"形态非法（33 位含 j，实测出现过）", "23c82fb98j92d22b2632be89eeb6cee8", want, false},
		{"空远端 ⇒ 不权威", "", want, false},
		{"空期望 ⇒ 不权威", want, "", false},
	}
	for _, tc := range cases {
		if got := md5Authoritative(tc.remote, tc.want); got != tc.expect {
			t.Errorf("%s: md5Authoritative(%q,%q)=%v, want %v", tc.name, tc.remote, tc.want, got, tc.expect)
		}
	}
}
