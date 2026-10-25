// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseMetaExtraHeader_Base64JSON 验证 base64(JSON) 解码 + 字段校验通过。
func TestParseMetaExtraHeader_Base64JSON(t *testing.T) {
	t.Parallel()
	obj := map[string]any{
		"code":   "NHDTC-254",
		"maker":  "NATURAL HIGH",
		"genres": []string{"痴女", "巨乳"},
	}
	b, _ := json.Marshal(obj)
	hdr := base64.StdEncoding.EncodeToString(b)
	got, err := parseMetaExtraHeader(hdr)
	if err != nil {
		t.Fatalf("base64 JSON 应通过: %v", err)
	}
	if got["code"] != "NHDTC-254" || got["maker"] != "NATURAL HIGH" {
		t.Fatalf("解码结果错: %v", got)
	}
}

// TestParseMetaExtraHeader_RawJSON 验证全 ASCII raw JSON 也可直接传（curl 调试友好）。
func TestParseMetaExtraHeader_RawJSON(t *testing.T) {
	t.Parallel()
	got, err := parseMetaExtraHeader(`{"code":"NHDTC-254","source":"hls"}`)
	if err != nil {
		t.Fatalf("raw JSON 应通过: %v", err)
	}
	if got["code"] != "NHDTC-254" {
		t.Fatalf("got: %v", got)
	}
}

// TestParseMetaExtraHeader_Empty 验证空头返回 nil（零回归）。
func TestParseMetaExtraHeader_Empty(t *testing.T) {
	t.Parallel()
	got, err := parseMetaExtraHeader("")
	if err != nil || got != nil {
		t.Fatalf("空头应 (nil,nil), got=%v err=%v", got, err)
	}
}

// TestParseMetaExtraHeader_InvalidField 验证未知字段被拒（fail-closed）。
func TestParseMetaExtraHeader_InvalidField(t *testing.T) {
	t.Parallel()
	_, err := parseMetaExtraHeader(`{"evil":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("未知字段应拒绝, err=%v", err)
	}
}

// TestParseMetaExtraHeader_Garbage 验证非法编码被拒。
func TestParseMetaExtraHeader_Garbage(t *testing.T) {
	t.Parallel()
	_, err := parseMetaExtraHeader("not-base64-not-json")
	if err == nil {
		t.Fatal("垃圾输入应拒绝")
	}
}

// TestParseMetaExtraHeader_TooLarge 验证超大 header 被拒。
func TestParseMetaExtraHeader_TooLarge(t *testing.T) {
	t.Parallel()
	_, err := parseMetaExtraHeader(`{"code":"` + strings.Repeat("x", 24<<10) + `"}`)
	if err == nil {
		t.Fatal("超大应拒绝")
	}
}
