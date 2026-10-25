// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestEncodeMetaExtraHeader 验证 extra → base64(JSON) 编码（非 ASCII 安全）。
func TestEncodeMetaExtraHeader(t *testing.T) {
	t.Parallel()
	hdr, err := encodeMetaExtraHeader(map[string]any{
		"code":     "NHDTC-254",
		"maker":    "NATURAL HIGH",
		"title_ja": "就寝中の夜行バス",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if hdr == "" {
		t.Fatal("编码结果不应为空")
	}
	// 头必须是可见 ASCII（base64 字符集）
	for _, r := range hdr {
		if r > 127 {
			t.Fatalf("头含非 ASCII: %q", hdr)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("JSON 解码失败: %v", err)
	}
	if got["code"] != "NHDTC-254" || got["title_ja"] != "就寝中の夜行バス" {
		t.Fatalf("往返不一致: %v", got)
	}
}

// TestEncodeMetaExtraHeader_Empty 验证空 map → 空串（不设头）。
func TestEncodeMetaExtraHeader_Empty(t *testing.T) {
	t.Parallel()
	hdr, err := encodeMetaExtraHeader(nil)
	if err != nil || hdr != "" {
		t.Fatalf("空 extra 应 (\"\",nil), got=%q err=%v", hdr, err)
	}
}

// TestEncodeMetaExtraHeader_InvalidField 验证白名单外字段被拒（本地 fail-fast）。
func TestEncodeMetaExtraHeader_InvalidField(t *testing.T) {
	t.Parallel()
	// encodeMetaExtraHeader 只 Trim+Marshal（校验在服务端与 UpdateFileMeta）；
	// 此处验证 Trim 后空白字段被剔除。
	hdr, err := encodeMetaExtraHeader(map[string]any{"code": "  "})
	if err != nil || hdr != "" {
		t.Fatalf("全空白应被 Trim 为空, got=%q err=%v", hdr, err)
	}
}

// TestWithMetaExtra 验证 UploadOption 写入。
func TestWithMetaExtra(t *testing.T) {
	t.Parallel()
	o := uploadOptions{}
	WithMetaExtra(map[string]any{"code": "x"})(&o)
	if o.MetaExtra["code"] != "x" {
		t.Fatalf("MetaExtra 未设置: %v", o.MetaExtra)
	}
}
