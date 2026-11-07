// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"slices"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestVerifyOwners_ExplicitOwnerIgnoresEnumeration 显式 owner 优先：不枚举租户（ListTenantIDs 不被调用）。
func TestVerifyOwners_ExplicitOwnerIgnoresEnumeration(t *testing.T) {
	t.Parallel()
	svc := New(Deps{ListTenantIDs: func() []string {
		t.Error("显式 owner 时不应枚举租户")
		return nil
	}})
	got := verifyOwners(svc, "bob")
	if !slices.Equal(got, []string{"bob"}) {
		t.Fatalf("verifyOwners(bob) = %v，want [bob]", got)
	}
}

// TestVerifyOwners_Enumeration 空 owner：磁盘扫描结果 + anonymous 兜底（未认证默认租户）。
func TestVerifyOwners_Enumeration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		listed []string
		want   []string
	}{
		{"空列表兜底 anonymous", nil, []string{storage.AnonymousOwner}},
		{"不含 anonymous 时追加", []string{"alice", "bob"}, []string{"alice", "bob", storage.AnonymousOwner}},
		{"已含 anonymous 不重复", []string{storage.AnonymousOwner, "alice"}, []string{storage.AnonymousOwner, "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Deps{ListTenantIDs: func() []string { return tc.listed }})
			got := verifyOwners(svc, "")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("verifyOwners(\"\") = %v，want %v", got, tc.want)
			}
		})
	}
}
