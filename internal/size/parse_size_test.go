// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package size

import (
	"math"
	"testing"
)

func TestParseSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want int64
		err  bool
	}{
		{"纯数字", "5368709120", 5 << 30, false},
		{"GiB", "5GiB", 5 << 30, false},
		{"MiB", "1MiB", 1 << 20, false},
		{"KiB", "1KiB", 1 << 10, false},
		{"B", "512B", 512, false},
		{"GB 十进制", "2GB", 2 * 1000 * 1000 * 1000, false},
		{"MB 十进制", "10MB", 10 * 1000 * 1000, false},
		{"KB 十进制", "3KB", 3 * 1000, false},
		{"TB", "1TB", 1000 * 1000 * 1000 * 1000, false},
		{"TiB", "1TiB", 1 << 40, false},
		{"小写", "5gib", 5 << 30, false},
		{"混合大小写", "5GiB", 5 << 30, false},
		{"空格", " 5GiB ", 5 << 30, false},
		{"无单位空格", "1024", 1024, false},
		{"0", "0", 0, false},
		{"0GiB", "0GiB", 0, false},
		{"小数", "1.5GiB", 3 << 29, false},
		{"小数十进制", "0.5GB", 500 * 1000 * 1000, false},
		{"单位大小写", "1MB", 1 * 1000 * 1000, false},
		{"裸K", "2K", 2 * 1000, false},
		{"裸M", "3M", 3 * 1000 * 1000, false},
		{"裸G", "4G", 4 * 1000 * 1000 * 1000, false},
		{"m 小写", "5m", 5 * 1000 * 1000, false},
		{"g 小写", "2g", 2 * 1000 * 1000 * 1000, false},
		{"k 小写", "8k", 8 * 1000, false},
		{"非法单位", "5XB", 0, true},
		{"非法字符", "abc", 0, true},
		{"空串", "", 0, true},
		{"负号", "-1", 0, true},
		{"负单位", "-1GiB", 0, true},
		{"溢出", "99999999999999999999GiB", 0, true},
		{"纯数字溢出", "99999999999999999999", 0, true},
		{"十进制小写单位", "1.5gb", 1500 * 1000 * 1000, false},
		{"带空格单位", "1 GiB", 1 << 30, false},
		{"制表符", "\t2MB\t", 2 * 1000 * 1000, false},
		{"极小", "1", 1, false},
		{"KiB大小写", "1KIB", 1 << 10, false},
		{"单个字符", "1", 1, false},
		{"单位无数字", "GiB", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSize(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSizeOverflow(t *testing.T) {
	t.Parallel()
	if _, err := ParseSize("9223372036854775807"); err != nil {
		t.Fatalf("MaxInt64 应可解析: %v", err)
	}
	if _, err := ParseSize("9223372036854775808"); err == nil {
		t.Fatal("超出 MaxInt64 应报错")
	}
	if _, err := ParseSize("9223372036854775807GiB"); err == nil {
		t.Fatal("MaxInt64 乘以 GiB 应溢出报错")
	}
	if _, err := ParseSize("1e18"); err == nil {
		t.Fatal("科学计数法应拒绝（防歧义）")
	}
	if got, err := ParseSize("9223372036854775807B"); err != nil || got != math.MaxInt64 {
		t.Fatalf("MaxInt64B 应解析为 MaxInt64: got=%d err=%v", got, err)
	}
}
