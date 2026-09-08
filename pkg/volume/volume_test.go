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

func TestAuthorize(t *testing.T) {
	denyOpen := Volume{Name: "a", ACL: ACL{Mode: ModeDeny, Owners: map[string]struct{}{"guest": {}}}}
	allowClosed := Volume{Name: "b", ACL: ACL{Mode: ModeAllow, Owners: map[string]struct{}{"alice": {}}}}
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

func TestOrderCandidates(t *testing.T) {
	vols := []Volume{{Name: "main"}, {Name: "disk2", Capacity: 100}, {Name: "disk3", Capacity: 200}}
	// spread：按 (cap-used) 降序——used 经回调查询（server 装配传卷池 Usage 闭包，保持包纯净）
	usage := map[string]int64{"disk2": 90, "disk3": 10}
	used := func(name string) int64 { return usage[name] }
	got := OrderCandidates(vols, ModeSpread, used)
	if got[0].Name != "disk3" {
		t.Fatalf("spread 应选 disk3(余 190) 优先, got %+v", names(got))
	}
	// prefer-default：main 恒最前，其余保持声明序
	got = OrderCandidates(vols, ModePreferDefault, used)
	if got[0].Name != "main" {
		t.Fatalf("prefer-default 首卷恒优先, got %+v", names(got))
	}
}
