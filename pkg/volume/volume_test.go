// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

import "testing"

// names 返回卷名列表（测试断言辅助）。
func names(vols []Volume) []string {
	out := make([]string, len(vols))
	for i := range vols {
		out[i] = vols[i].Name
	}
	return out
}

// namesEqual 断言卷名序列与期望一致（顺序敏感）。
func namesEqual(got []Volume, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Name != want[i] {
			return false
		}
	}
	return true
}

func TestAuthorize(t *testing.T) {
	denyOpen := Volume{Name: "a", ACL: ACL{Mode: ModeDeny, Owners: map[string]struct{}{"guest": {}}}}
	allowClosed := Volume{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}
	zeroVal := Volume{Name: "z", ACL: ACL{}} // Mode=="" 零值/未配
	unknownMode := Volume{Name: "u", ACL: ACL{Mode: "bogus"}}
	cases := []struct {
		name  string
		vol   Volume
		owner string
		want  bool
	}{
		{"deny 未在名单→开放", denyOpen, "alice", true},
		{"deny 命中黑名单→拒", denyOpen, "guest", false},
		{"allow 白名单命中→放行", allowClosed, "alice", true},
		{"allow 未命中→拒", allowClosed, "bob", false},
		{"零值 ACL（Mode==\"\"）→ 默认开放", zeroVal, "alice", true},
		{"未知非空 mode → fail-closed 拒", unknownMode, "alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vol.Authorize(tc.owner); got != tc.want {
				t.Fatalf("Authorize(%q)=%v want %v", tc.owner, got, tc.want)
			}
		})
	}
}

func TestAllowedVolumes(t *testing.T) {
	vols := []Volume{
		{Name: "a", ACL: ACL{Mode: ModeDeny}},                                            // 开放
		{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}, // 仅 alice
		{Name: "c", ACL: ACL{Mode: ModeAllow}},                                           // allow 空=全拒
	}
	got := AllowedVolumes(vols, "bob")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("bob 应仅见卷 a, got %+v", names(got))
	}
}

func TestAllowedVolumes_KeepsDeclarationOrder(t *testing.T) {
	vols := []Volume{
		{Name: "a", ACL: ACL{Mode: ModeDeny}},                                            // 开放
		{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}, // 仅 alice
		{Name: "c", ACL: ACL{Mode: ModeDeny, Owners: map[string]struct{}{"alice": {}}}},  // alice 命中黑名单
	}
	got := AllowedVolumes(vols, "alice")
	if !namesEqual(got, "a", "b") {
		t.Fatalf("alice 应见 [a b] 且保持声明序, got %+v", names(got))
	}
}

func TestDefaultVolume(t *testing.T) {
	if got := DefaultVolume([]Volume{{Name: "main"}, {Name: "disk2", Capacity: 100}}); got.Name != "main" {
		t.Fatalf("非空列表应返回首卷, got %+v", got)
	}
	if got := DefaultVolume(nil); got.Name != "<none>" {
		t.Fatalf("空列表应返回 Name==\"<none>\", got %+v", got)
	}
}

func TestOrderCandidates(t *testing.T) {
	// spread 必须扣减 used：disk2 容量(300) 虽大于 disk3(200)，但 used 高（余 10 < disk3 余 200）
	// → 丢弃 used（退化为按容量排序）的实现会错选 disk2，故锁死「减 used」。main 不限容量余量按
	// 0 计、排末尾（溢出兜底）。
	vols := []Volume{{Name: "main"}, {Name: "disk2", Capacity: 300}, {Name: "disk3", Capacity: 200}}
	usage := map[string]int64{"disk2": 290, "disk3": 0}
	used := func(name string) int64 { return usage[name] }
	got := OrderCandidates(vols, ModeSpread, used)
	if !namesEqual(got, "disk3", "disk2", "main") {
		t.Fatalf("spread 应按余量降序 disk3>disk2>main, got %+v", names(got))
	}
	// prefer-default：main 恒最前，其余保持声明序
	got = OrderCandidates(vols, ModePreferDefault, used)
	if !namesEqual(got, "main", "disk2", "disk3") {
		t.Fatalf("prefer-default 应保持声明序 main>disk2>disk3, got %+v", names(got))
	}
}

func TestOrderCandidates_SpreadNilUsed(t *testing.T) {
	// used==nil 不 panic，全部视为 0 → 有界卷按 Capacity 降序。
	vols := []Volume{{Name: "v1", Capacity: 100}, {Name: "v2", Capacity: 300}, {Name: "v3", Capacity: 200}}
	got := OrderCandidates(vols, ModeSpread, nil)
	if !namesEqual(got, "v2", "v3", "v1") {
		t.Fatalf("nil used 应按容量降序 v2>v3>v1, got %+v", names(got))
	}
}
