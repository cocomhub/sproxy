// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// metaextra_header.go 解析上传请求头 `X-File-Meta-Extra`（方案①：随上传原子写入 sidecar）。
//
// 编码：base64(标准, 带 padding) 包裹的 JSON 对象——HTTP 头只允许可见 ASCII，中文必须
// 编码传输（raw JSON 含非 ASCII 会被 HTTP 层拒绝）。同时兼容**纯 JSON**（全 ASCII 时
// 客户端可直接传，便于 curl 调试）。
//
// 安全：解码后大小 ≤ 16 KiB（防滥用）、必须是对象、逐字段白名单校验（pkg/files/meta.Extra 白名单）。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// headerFileMetaExtra 是随上传提交自定义元数据的请求头。
const headerFileMetaExtra = "X-File-Meta-Extra"

// maxMetaExtraRaw 是解码前 header 长度上限（base64 膨胀 4/3 → 16 KiB JSON ≈ 22 KiB）。
const maxMetaExtraRaw = 24 << 10

// parseMetaExtraHeader 解析 X-File-Meta-Extra 头：base64(JSON) 或 raw JSON。
// 空头 → (nil, nil)。非法编码/解码失败/非对象/字段校验失败 → 错误（调用方 400）。
func parseMetaExtraHeader(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxMetaExtraRaw {
		return nil, fmt.Errorf("header 超长（%d > %d）", len(raw), maxMetaExtraRaw)
	}
	payload, err := decodeMetaExtraPayload(raw)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if obj == nil {
		return nil, errors.New("必须是 JSON 对象")
	}
	obj = meta.TrimExtra(obj)
	if err := meta.ValidateExtra(obj); err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return nil, nil
	}
	return obj, nil
}

// decodeMetaExtraPayload 尝试 base64 解码；失败则视为 raw JSON（全 ASCII 场景）。
func decodeMetaExtraPayload(raw string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return b, nil
	}
	// 兼容 URL-safe 无 padding
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "=")); err == nil {
		return b, nil
	}
	if strings.HasPrefix(raw, "{") {
		return []byte(raw), nil
	}
	return nil, errors.New("既不是合法 base64 也不是 JSON 对象")
}
