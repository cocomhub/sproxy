// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// 本文件是 BaiduPCS-Go（Apache-2.0，https://github.com/qjfoidnh/BaiduPCS-Go）
// pcsutil/jsonhelper/jsonhelper.go 的裁剪版本：jsoniter 替换为标准库 encoding/json。

// Package jsonhelper 提供 JSON 编解码工具（裁剪自 BaiduPCS-Go）。
package jsonhelper

import (
	"encoding/json"
	"io"
)

// UnmarshalData 将 r 中的 json 格式的数据, 解析到 data。
func UnmarshalData(r io.Reader, data interface{}) error {
	return json.NewDecoder(r).Decode(data)
}

// MarshalData 将 data, 生成 json 格式的数据, 写入 w 中。
func MarshalData(w io.Writer, data interface{}) error {
	return json.NewEncoder(w).Encode(data)
}
