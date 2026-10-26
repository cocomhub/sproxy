// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// extra.go 定义随上传写入 .meta sidecar 的自定义元数据（Extra）schema 与校验。
//
// 设计（用户裁定 2026-10-24）：上传/补录时把「站点元数据 + 溯源信息」写入
// FileMeta.Extra（已有扩展字段）。本包是**字段白名单 + 类型 + 长度**的单一事实源，
// 防任意键注入（安全）、防超大对象（完整性）、保证可检索（稳定键）。
//
// 字段分档：
//
//	A（内容）：code/maker/maker_slug/label/series/actress/genres/director/release_date/
//	           title_ja/title_cn/page_url/site
//	B（溯源）：origin_name/source/share_url/share_id/share_file_name/
//	           share_file_id/share_hash/downloaded_at/mirror_urls/nfo_ref
//	C（可选）：duration_sec/resolution/uncensored/has_cn_sub/subtitle_files
package meta

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// maxExtraBytes 是 Extra 序列化后的大小上限（防滥用：16 KiB 足够承载全部字段）。
const maxExtraBytes = 16 << 10

// CustomKeyPrefix 是「客户端自定义扩展字段」的键前缀。
//
// 设计（用户裁定 2026-10-26）：除固定白名单（A/B/C 档，跨站点通用语义）外，客户端/站点
// 可能需要携带自身特有字段（新站点元数据、实验字段等）。若每次新增都要改服务端白名单，
// 会形成「客户端扩展 → 服务端发版」的强耦合。约定：**以 `x_` 前缀的键视为自定义扩展**——
//   - 不做键白名单校验（任意键名），但仍受类型/长度/总大小约束（防注入与膨胀）；
//   - 允许类型：string / bool / int64 / []string / float64（JSON 可表达的基本标量）；
//   - 单键长度与总量受 maxExtraBytes 约束；键名限 `x_` + [a-z0-9_]（≤64 字符）。
//
// 与服务端白名单字段并存：白名单字段走严格语义校验，`x_` 字段走通用约束。
const CustomKeyPrefix = "x_"

// FieldSpec 描述一个字段的类型与约束。
type FieldSpec struct {
	Type     string   // string | []string | int64 | bool
	MaxLen   int      // string/数组元素最大长度
	Optional bool     // 是否必填（当前全部可选）
	Allowed  []string // string 枚举（空 = 任意）
}

// spec 是字段白名单（键 → 规格）。新增字段须在此登记（唯一事实源）。
var spec = map[string]FieldSpec{
	// A 档：内容元数据
	"code":         {Type: "string", MaxLen: 32},
	"maker":        {Type: "string", MaxLen: 64},
	"maker_slug":   {Type: "string", MaxLen: 64},
	"label":        {Type: "string", MaxLen: 64},
	"series":       {Type: "string", MaxLen: 128},
	"actress":      {Type: "[]string", MaxLen: 64},
	"actress_cn":   {Type: "[]string", MaxLen: 64},
	"genres":       {Type: "[]string", MaxLen: 64},
	"genres_cn":    {Type: "[]string", MaxLen: 64},
	"director":     {Type: "string", MaxLen: 64},
	"release_date": {Type: "string", MaxLen: 16},
	"title_ja":     {Type: "string", MaxLen: 400},
	"title_cn":     {Type: "string", MaxLen: 400},
	"page_url":     {Type: "string", MaxLen: 512},
	"site":         {Type: "string", MaxLen: 32}, // 站点名（如视频站/网盘站），自由字符串
	// B 档：溯源
	"origin_name":     {Type: "string", MaxLen: 512},
	"source":          {Type: "string", MaxLen: 32, Allowed: []string{"cloud_share", "http_direct", "hls", "local_upload"}},
	"share_url":       {Type: "string", MaxLen: 1024}, // 分享链接（网盘/中转等，不绑定厂商）
	"share_id":        {Type: "string", MaxLen: 128},
	"share_file_name": {Type: "string", MaxLen: 512}, // 分享内目标文件名
	"share_file_id":   {Type: "string", MaxLen: 128},
	"share_hash":      {Type: "string", MaxLen: 128}, // 分享内文件哈希
	"downloaded_at":   {Type: "string", MaxLen: 32},
	"mirror_urls":     {Type: "[]string", MaxLen: 1024},
	"nfo_ref":         {Type: "string", MaxLen: 128},
	// C 档：可选
	"duration_sec":   {Type: "int64"},
	"resolution":     {Type: "string", MaxLen: 16, Allowed: []string{"480p", "720p", "1080p", "4k"}},
	"uncensored":     {Type: "bool"},
	"has_cn_sub":     {Type: "bool"},
	"has_en_sub":     {Type: "bool"},
	"cover_url":      {Type: "string", MaxLen: 512},
	"subtitle_files": {Type: "[]string", MaxLen: 512},
}

// ValidateExtra 校验 Extra 对象：键白名单（+ `x_` 自定义扩展）+ 类型 + 长度 + 总大小。
// 返回错误即拒绝写入（fail-closed：不合法不落 meta）。
func ValidateExtra(extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return fmt.Errorf("meta: extra 序列化失败: %w", err)
	}
	if len(b) > maxExtraBytes {
		return fmt.Errorf("meta: extra 超上限 %d B（实际 %d）", maxExtraBytes, len(b))
	}
	for k, v := range extra {
		if err := validateOneKey(k, v); err != nil {
			return err
		}
	}
	return nil
}

// validateOneKey 校验单个 extra 键：自定义扩展（x_ 前缀）或白名单字段。
func validateOneKey(k string, v any) error {
	if strings.HasPrefix(k, CustomKeyPrefix) {
		// 客户端自定义扩展：键名限 `x_` + [a-z0-9_]，类型限基本标量，长度受总量约束。
		if !isCustomKey(k) {
			return fmt.Errorf("meta: extra 自定义键 %q 非法（须 x_ 前缀 + 小写字母数字下划线，≤64 字符）", k)
		}
		return validateCustomValue(k, v)
	}
	fs, ok := spec[k]
	if !ok {
		return fmt.Errorf("meta: extra 未知字段 %q（白名单外；自定义扩展请用 %s 前缀）", k, CustomKeyPrefix)
	}
	return validateField(k, v, fs)
}

// isCustomKey 校验自定义扩展键名（x_ 前缀 + [a-z0-9_]，≤64）。
func isCustomKey(k string) bool {
	if !strings.HasPrefix(k, CustomKeyPrefix) || len(k) > 64 || len(k) == len(CustomKeyPrefix) {
		return false
	}
	for _, r := range k[len(CustomKeyPrefix):] {
		if !isCustomKeyChar(r) {
			return false
		}
	}
	return true
}

// isCustomKeyChar 判断字符是否允许出现在自定义扩展键名中（小写字母/数字/下划线）。
func isCustomKeyChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
}

// validateCustomValue 校验自定义扩展字段值（string/bool/数字/[]string/[]any 标量）。
func validateCustomValue(k string, v any) error {
	switch t := v.(type) {
	case string:
		if len(t) > maxExtraBytes {
			return fmt.Errorf("meta: extra 自定义字段 %q 超长", k)
		}
		return nil
	case bool, float64, json.Number, int64:
		return nil
	case []string, []any:
		return nil
	default:
		return fmt.Errorf("meta: extra 自定义字段 %q 类型不支持（%T），允许 string/bool/数字/[]string", k, v)
	}
}

// validateField 校验单字段类型/长度/枚举。
func validateField(k string, v any, fs FieldSpec) error {
	switch fs.Type {
	case "string":
		return validateStringField(k, v, fs)
	case "[]string":
		return validateStringSliceField(k, v, fs)
	case "int64":
		if !isNumber(v) {
			return fmt.Errorf("meta: extra 字段 %q 应为数字", k)
		}
	case "bool":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("meta: extra 字段 %q 应为 bool", k)
		}
	default:
		return fmt.Errorf("meta: extra 未支持类型 %q", fs.Type)
	}
	return nil
}

// validateStringField 校验 string 字段（长度 + 枚举）。
func validateStringField(k string, v any, fs FieldSpec) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("meta: extra 字段 %q 应为 string", k)
	}
	if len(s) > fs.MaxLen {
		return fmt.Errorf("meta: extra 字段 %q 超长 %d（上限 %d）", k, len(s), fs.MaxLen)
	}
	if len(fs.Allowed) > 0 && !contains(fs.Allowed, s) {
		return fmt.Errorf("meta: extra 字段 %q 值 %q 不在枚举内", k, s)
	}
	return nil
}

// validateStringSliceField 校验 []string 字段（元素长度；兼容 JSON 反序列化 []any）。
func validateStringSliceField(k string, v any, fs FieldSpec) error {
	switch t := v.(type) {
	case []string:
		return checkElems(k, t, fs.MaxLen)
	case []any:
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("meta: extra 字段 %q 数组元素应为 string", k)
			}
			if len(s) > fs.MaxLen {
				return fmt.Errorf("meta: extra 字段 %q 元素超长 %d（上限 %d）", k, len(s), fs.MaxLen)
			}
		}
		return nil
	default:
		return fmt.Errorf("meta: extra 字段 %q 应为 []string", k)
	}
}

// checkElems 校验 []string 元素长度。
func checkElems(k string, arr []string, maxLen int) error {
	for _, s := range arr {
		if len(s) > maxLen {
			return fmt.Errorf("meta: extra 字段 %q 元素超长 %d（上限 %d）", k, len(s), maxLen)
		}
	}
	return nil
}

// isNumber 判断是否 JSON 数字（float64）或整型。
func isNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// Trim 对字符串字段做 trim + 去空（避免空白键污染 meta）。
func TrimExtra(extra map[string]any) map[string]any {
	out := make(map[string]any, len(extra))
	for k, v := range extra {
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				out[k] = s
			}
		default:
			out[k] = v
		}
	}
	return out
}
