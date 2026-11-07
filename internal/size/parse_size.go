// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package size

import "github.com/cocomhub/sproxy/pkg/units/sizex"

// ParseSize 把人类可读字节大小解析为 int64 字节数（**委托** pkg/units/sizex）。
//
// 历史：本文件曾与 pkg/units/sizex 各自实现一份**逐字相同**的解析器（单位表、
// splitSizeParts、int/float 两条路径），只是错误前缀不同（"size:" / "sizex:"）。
// 两份等价实现在配置口径上分叉的风险是真实的（例如一处支持 "1.5GiB" 另一处不支持），
// 而 AGENTS.md 硬规则 19 已指定 pkg/units/sizex.ByteSize 为字节大小的唯一实现。
//
// 现在本包只保留**分块/请求体上限常量**（size.go），解析委托给唯一实现。
func ParseSize(s string) (int64, error) {
	return sizex.ParseSize(s)
}
