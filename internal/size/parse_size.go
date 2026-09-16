// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package size

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 二进制（IEC 60027-2）与十进制（SI）单位乘数。单位大小写不敏感。
// 支持：B/K/KB/KiB、M/MB/MiB、G/GB/GiB、T/TB/TiB；1 K = 1 KB = 1000 B，
// 1 Ki = 1024 B（下同）。P/E 级别未纳入（配额/卷容量量级内 T 足够，超界由
// 溢出检测兜底）。
var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"kib", 1 << 10},
	{"kb", 1000},
	{"ki", 1 << 10},
	{"k", 1000},
	{"mib", 1 << 20},
	{"mb", 1000 * 1000},
	{"mi", 1 << 20},
	{"m", 1000 * 1000},
	{"gib", 1 << 30},
	{"gb", 1000 * 1000 * 1000},
	{"gi", 1 << 30},
	{"g", 1000 * 1000 * 1000},
	{"tib", 1 << 40},
	{"tb", 1000 * 1000 * 1000 * 1000},
	{"ti", 1 << 40},
	{"t", 1000 * 1000 * 1000 * 1000},
	{"b", 1},
}

// ParseSize 把人类可读字节大小解析为 int64 字节数。
//
// 支持的格式（单位大小写不敏感，可带前导/尾随空白，数字与单位间可有空格）：
//
//	纯数字           → 字节（"5368709120" = 5368709120 B）
//	十进制 SI 单位   → "1KB"=1000、"1MB"=1000^2、"1GB"、"1TB"（"K"/"M"/"G"/"T" 裸后缀同义）
//	二进制 IEC 单位  → "1KiB"=1024、"1MiB"、"1GiB"、"1TiB"
//	小数             → "1.5GiB"=1536MiB、"0.5GB"=500MB
//
// 约束：不接受负数、科学计数法、未知单位；解析结果超出 int64 范围时报错。
// 纯数字超出 int64 同样报错（与 strconv.ParseInt 语义一致）。
func ParseSize(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("size: 空字符串")
	}
	if strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("size: 配额/容量不允许负数 %q", s)
	}
	if strings.ContainsAny(raw, "eE") {
		// 科学计数法（1e6）拒绝：与人类可读大小格式不一致，且容易与单位字母混淆。
		return 0, fmt.Errorf("size: 不支持科学计数法 %q", s)
	}

	numPart, unitPart, _ := strings.Cut(raw, " ")
	numPart = strings.TrimSpace(numPart)
	unitPart = strings.TrimSpace(unitPart)
	// 数字与单位间没有空格（"5GiB"）：分离数字与单位边界。
	if unitPart == "" {
		i := 0
		for i < len(numPart) && (numPart[i] >= '0' && numPart[i] <= '9' || numPart[i] == '.') {
			i++
		}
		if i > 0 && i < len(numPart) {
			unitPart = strings.TrimSpace(numPart[i:])
			numPart = numPart[:i]
		}
	}

	mult := int64(1)
	if unitPart != "" {
		u := strings.ToLower(unitPart)
		found := false
		for _, unit := range sizeUnits {
			if unit.suffix == u {
				mult = unit.mult
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("size: 未知单位 %q（支持 B/K/KB/KiB/M/MB/MiB/G/GB/GiB/T/TB/TiB）", unitPart)
		}
	}

	if numPart == "" {
		return 0, fmt.Errorf("size: 缺少数字部分 %q", s)
	}

	if strings.Contains(numPart, ".") {
		f, err := strconv.ParseFloat(numPart, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("size: 非法数字 %q", numPart)
		}
		if f < 0 {
			return 0, fmt.Errorf("size: 配额/容量不允许负数 %q", s)
		}
		// 乘法定界：先算 f*mult 再判溢出（乘后 > MaxInt64 即溢出）。
		v := f * float64(mult)
		if v > float64(math.MaxInt64) {
			return 0, fmt.Errorf("size: 数值溢出 int64: %q", s)
		}
		return int64(v), nil
	}

	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size: 非法数字 %q: %w", numPart, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size: 配额/容量不允许负数 %q", s)
	}
	if mult == 1 {
		return n, nil
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size: 数值溢出 int64: %q", s)
	}
	return n * mult, nil
}
