// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sizex 是字节大小单位抽象（pkg/units 子包，2026-10-03 用户裁决）。
//
// 自包含：ByteSize 类型 + ParseSize 解析 + UnmarshalText/MarshalText 序列化，
// 不依赖 internal/size（internal/size 保留不动，后续单独 PR 统一迁到本包）。
// 独立于 pkg/server（pkg/volume 等不反向依赖 server）与 cmd 子 module
// （go.work 多 module 均可引用）。
//
// ByteSize 支持两种解码路径：
//   - YAML/viper：实现 encoding.TextUnmarshaler——yaml.v3 对实现了该接口的命名 int
//     类型优先调用 UnmarshalText（数字与字符串原文都会传入）；viper 侧由
//     cmd/sproxy/internal/sproxycfg 的 decode hook 委托 UnmarshalText；
//   - 直接赋值（测试/代码构造）：仍可用整数。
//
// 解析语义与格式全集见 ParseSize。
package sizex

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ByteSize 是人类可读字节大小的解码类型（int64 底层）。
// 纯数字=字节（"5368709120"）、SI 单位（"5GB"=5×10⁹）、IEC 单位（"5GiB"=5×2³⁰）；
// 大小写不敏感、可带小数。零值与整数运算兼容（int64 命名类型）。
type ByteSize int64

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
		return 0, fmt.Errorf("sizex: 空字符串")
	}
	if strings.HasPrefix(raw, "-") {
		return 0, fmt.Errorf("sizex: 配额/容量不允许负数 %q", s)
	}
	if strings.ContainsAny(raw, "eE") {
		// 科学计数法（1e6）拒绝：与人类可读大小格式不一致，且容易与单位字母混淆。
		return 0, fmt.Errorf("sizex: 不支持科学计数法 %q", s)
	}

	numPart, unitPart := splitSizeParts(raw)
	mult, err := resolveSizeUnit(unitPart)
	if err != nil {
		return 0, err
	}
	if numPart == "" {
		return 0, fmt.Errorf("sizex: 缺少数字部分 %q", s)
	}

	if strings.Contains(numPart, ".") {
		return parseSizeFloat(s, numPart, mult)
	}
	return parseSizeInt(s, numPart, mult)
}

// splitSizeParts 把原始串拆分为数字部分与单位部分（支持空格分隔与无空格边界）。
func splitSizeParts(raw string) (string, string) {
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
	return numPart, unitPart
}

// resolveSizeUnit 把单位串解析为乘数（大小写不敏感）；未知单位返回哨兵错误。
func resolveSizeUnit(unitPart string) (int64, error) {
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
			return 0, fmt.Errorf("sizex: 未知单位 %q（支持 B/K/KB/KiB/M/MB/MiB/G/GB/GiB/T/TB/TiB）", unitPart)
		}
	}
	return mult, nil
}

// parseSizeFloat 解析小数部分并乘单位定界（非法数字/负数/溢出对应哨兵错误）。
func parseSizeFloat(s, numPart string, mult int64) (int64, error) {
	f, err := strconv.ParseFloat(numPart, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("sizex: 非法数字 %q", numPart)
	}
	if f < 0 {
		return 0, fmt.Errorf("sizex: 配额/容量不允许负数 %q", s)
	}
	// 乘法定界：先算 f*mult 再判溢出（乘后 > MaxInt64 即溢出）。
	v := f * float64(mult)
	if v > float64(math.MaxInt64) {
		return 0, fmt.Errorf("sizex: 数值溢出 int64: %q", s)
	}
	return int64(v), nil
}

// parseSizeInt 解析整数部分并乘单位定界（非法数字/负数/溢出对应哨兵错误）。
func parseSizeInt(s, numPart string, mult int64) (int64, error) {
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sizex: 非法数字 %q: %w", numPart, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("sizex: 配额/容量不允许负数 %q", s)
	}
	if mult == 1 {
		return n, nil
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("sizex: 数值溢出 int64: %q", s)
	}
	return n * mult, nil
}

// UnmarshalText 实现 encoding.TextUnmarshaler：接受纯数字与带单位字符串。
func (b *ByteSize) UnmarshalText(text []byte) error {
	v, err := ParseSize(string(text))
	if err != nil {
		return err
	}
	*b = ByteSize(v)
	return nil
}

// UnmarshalJSON 实现 json.Unmarshaler（2026-10-03 补充）：接受 JSON 字符串（"8MiB"）
// 与 JSON 数字（8388608）双形态——encoding/json 对命名 int 类型默认只接受数字，加
// UnmarshalJSON 使字符串形态（配置以 "8MiB" 写出/API JSON 传输）也能解析。非法输入
// fail-closed（含负数）。
func (b *ByteSize) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("sizex: 空 JSON")
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		v, err := ParseSize(s)
		if err != nil {
			return err
		}
		*b = ByteSize(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("sizex: JSON 数字解析失败 %q: %w", data, err)
	}
	if n < 0 {
		return fmt.Errorf("sizex: 配额/容量不允许负数 %q", data)
	}
	*b = ByteSize(n)
	return nil
}

// MarshalJSON 实现 json.Marshaler：数字序列化（"8388608"）——JSON 传输/落盘用纯数字
// （与 MarshalText 语义一致），避免字符串形态造成双轨歧义。
func (b ByteSize) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(b), 10)), nil
}

// MarshalText 实现 encoding.TextMarshaler：序列化为整数（字节）形式。
// SaveConfig 输出与旧版格式保持一致（纯数字），避免引入解析歧义。
func (b ByteSize) MarshalText() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(b), 10)), nil
}

// Bytes 返回 int64 字节数（便捷访问器；int64 命名类型直接转换亦可，显式访问器更友好）。
func (b ByteSize) Bytes() int64 { return int64(b) }

// String 返回人类可读字节大小（fmt/log 友好）。单位采用 IEC（2^30 系）：
// >=1GiB → "8GiB"、>=1MiB → "800MiB"、>=1KiB → "1024KiB"、否则 "1234B"。
// 负值/零原样输出（负数在配额域被 ParseSize 拒绝，这里仅防御性展示）。
func (b ByteSize) String() string {
	v := int64(b)
	if v == 0 {
		return "0B"
	}
	if v < 0 {
		return strconv.FormatInt(v, 10)
	}
	switch {
	case v >= 1<<40:
		return strconv.FormatInt(v/(1<<40), 10) + "TiB"
	case v >= 1<<30:
		return strconv.FormatInt(v/(1<<30), 10) + "GiB"
	case v >= 1<<20:
		return strconv.FormatInt(v/(1<<20), 10) + "MiB"
	case v >= 1<<10:
		return strconv.FormatInt(v/(1<<10), 10) + "KiB"
	default:
		return strconv.FormatInt(v, 10) + "B"
	}
}

// Set 实现 pflag.Value / flag.Value：支持 CLI `--max-file-bytes 8MiB` 直接绑定。
func (b *ByteSize) Set(s string) error {
	return b.UnmarshalText([]byte(s))
}

// Type 实现 pflag.Value 的类型名（CLI help 展示 "bytes"）。
func (b *ByteSize) Type() string { return "bytes" }
