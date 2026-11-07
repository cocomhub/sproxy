// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sizex

import (
	"encoding/json"
	"math"
	"testing"
)

// TestParseSize 覆盖 ParseSize 全部格式（与 internal/size 语料一致：本包为后续
// 统一迁移的权威实现，语义与 internal/size 逐例对齐）。
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

// TestParseSizeOverflow 边界溢出与科学计数法拒绝。
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

// TestByteSizeRoundtrip 验证 UnmarshalText/MarshalText 双轨（YAML/viper decode hook
// 依赖）：字符串与纯数字均可解析、MarshalText 输出纯数字（SaveConfig 旧版格式一致）。
func TestByteSizeRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want ByteSize
	}{
		{"8MiB", 8 << 20},
		{"100GiB", 100 << 30},
		{"5368709120", 5 << 30},
		{"0", 0},
		{"1.5GiB", ByteSize(3 << 29)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			var b ByteSize
			if err := b.UnmarshalText([]byte(tc.in)); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", tc.in, err)
			}
			if b != tc.want {
				t.Fatalf("UnmarshalText(%q) = %d, want %d", tc.in, b, tc.want)
			}
			got, err := b.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			if string(got) != itoa(int64(tc.want)) {
				t.Errorf("MarshalText = %s, want %d（纯数字形式）", got, int64(tc.want))
			}
		})
	}
	// 非法解析 fail-closed。
	var b ByteSize
	if err := b.UnmarshalText([]byte("abc")); err == nil {
		t.Error("非法单位应报错")
	}
}

// TestByteSizeJSON（2026-10-03 补充）：json.Marshal → 纯数字；json.Unmarshal 接受
// 字符串（"8MiB"）与数字（8388608）双形态；非法输入（"abc"/"8XB"/负数）fail-closed。
func TestByteSizeJSON(t *testing.T) {
	t.Parallel()
	// Marshal：纯数字序列化。
	b := ByteSize(8 << 20)
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != "8388608" {
		t.Errorf("Marshal(8MiB) = %s, want 8388608", got)
	}
	// Unmarshal：字符串形态。
	var bs ByteSize
	if err := json.Unmarshal([]byte(`"8MiB"`), &bs); err != nil {
		t.Fatalf("Unmarshal string: %v", err)
	}
	if bs != 8<<20 {
		t.Errorf("Unmarshal(\"8MiB\") = %d, want %d", bs, 8<<20)
	}
	// Unmarshal：数字形态。
	var bn ByteSize
	if err := json.Unmarshal([]byte(`8388608`), &bn); err != nil {
		t.Fatalf("Unmarshal number: %v", err)
	}
	if bn != 8<<20 {
		t.Errorf("Unmarshal(8388608) = %d, want %d", bn, 8<<20)
	}
	// 非法输入 fail-closed。
	for _, bad := range []string{`"abc"`, `"8XB"`, `"-1"`, `-1`} {
		var x ByteSize
		if err := json.Unmarshal([]byte(bad), &x); err == nil {
			t.Errorf("Unmarshal(%s) 应报错", bad)
		}
	}
}

// TestByteSizeString（2026-10-03 补充）：人类可读 String()（fmt/log 友好）。
func TestByteSizeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   ByteSize
		want string
	}{
		{ByteSize(8 << 20), "8MiB"},
		{ByteSize(1 << 30), "1GiB"},
		{ByteSize(1 << 40), "1TiB"},
		{ByteSize(1 << 10), "1KiB"},
		{ByteSize(512), "512B"},
		{ByteSize(0), "0B"},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", int64(c.in), got, c.want)
		}
	}
}

// TestByteSizeFlag（2026-10-03 补充）：pflag/flag.Value 接口（CLI --max-file-bytes 8MiB）。
func TestByteSizeFlag(t *testing.T) {
	t.Parallel()
	var b ByteSize
	if err := b.Set("8MiB"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if b != 8<<20 {
		t.Errorf("Set(8MiB) = %d, want %d", b, 8<<20)
	}
	if b.Type() != "bytes" {
		t.Errorf("Type() = %q, want bytes", b.Type())
	}
	if err := b.Set("abc"); err == nil {
		t.Error("Set(非法) 应报错")
	}
	// Bytes() 便捷访问器。
	if b.Bytes() != 8<<20 {
		t.Errorf("Bytes() = %d, want %d", b.Bytes(), 8<<20)
	}
}

// itoa 避免测试里为纯数字序列化额外 import strconv（MarshalText 语义断言用）。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
