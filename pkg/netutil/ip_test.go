// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"net"
	"testing"
)

func TestParseIPNets(t *testing.T) {
	t.Parallel()
	nets := ParseIPNets([]string{"10.0.0.0/8", "192.168.1.5", "::1", "bad"})
	if len(nets) != 3 {
		t.Fatalf("ParseIPNets 应跳过非法条目并保留 3 条，got %d", len(nets))
	}
	if !nets[1].Contains(net.ParseIP("192.168.1.5")) {
		t.Error("纯 IPv4 应按 /32 归一")
	}
	if ones, _ := nets[1].Mask.Size(); ones != 32 {
		t.Errorf("纯 IPv4 掩码应为 /32，got /%d", ones)
	}
	if ones, _ := nets[2].Mask.Size(); ones != 128 {
		t.Errorf("纯 IPv6 掩码应为 /128，got /%d", ones)
	}
}

func TestAllowIPsCoverLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"both", []string{"127.0.0.1", "::1"}, true},
		{"cidr both", []string{"127.0.0.0/8", "::1/128"}, true},
		{"v4 only", []string{"127.0.0.1"}, false},
		{"v6 only", []string{"::1"}, false},
		{"empty", nil, false},
		{"unrelated", []string{"10.0.0.0/8"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AllowIPsCoverLoopback(tc.in); got != tc.want {
				t.Errorf("AllowIPsCoverLoopback(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
